package ui

import (
	"bytes"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/Bios-Marcel/discordemojimap"
	"github.com/Bios-Marcel/goclipimg"

	"github.com/atotto/clipboard"

	"github.com/Bios-Marcel/cordless/commands"
	"github.com/Bios-Marcel/cordless/config"
	"github.com/Bios-Marcel/cordless/discordutil"
	"github.com/Bios-Marcel/cordless/maths"
	"github.com/Bios-Marcel/cordless/readstate"
	"github.com/Bios-Marcel/cordless/scripting"
	"github.com/Bios-Marcel/cordless/scripting/js"
	"github.com/Bios-Marcel/cordless/shortcuts"
	"github.com/Bios-Marcel/cordless/times"
	"github.com/Bios-Marcel/cordless/ui/tviewutil"
	"github.com/Bios-Marcel/cordless/util/fuzzy"
	"github.com/Bios-Marcel/discordgo"
	"github.com/Bios-Marcel/tview"
	"github.com/gdamore/tcell"
	"github.com/gen2brain/beeep"
)

const (
	guildPageName    = "Guilds"
	privatePageName  = "Private"
	userInactiveTime = 10 * time.Second
)

var (
	//emojiRegex is used to find emojicodes for custom emojis. The !? part
	// after the first double colon exists in order as a flag to tell cordless
	// to use the emoji as a custom emoji, since there can be clashes with for
	// example :joy:, which is a default emoji code.
	emojiRegex = regexp.MustCompile("(m?)(^|[^<]):!?.+?:")
)

// Window is basically the whole application, as it contains all the
// components and the necessary global state.
type Window struct {
	app               *tview.Application
	middleContainer   *tview.Flex
	rootContainer     *tview.Flex
	dialogReplacement *tview.Flex
	dialogButtonBar   *tview.Flex
	dialogTextView    *tview.TextView
	currentContainer  tview.Primitive

	leftArea    *tview.Pages
	guildList   *GuildList
	channelTree *ChannelTree
	privateList *PrivateChatList

	chatArea         *tview.Flex
	chatView         *ChatView
	messageContainer tview.Primitive
	messageInput     *Editor

	editingMessageID *string

	userList *UserTree

	session *discordgo.Session

	selectedGuildNode   *tview.TreeNode
	previousGuildNode   *tview.TreeNode
	selectedGuild       *discordgo.Guild
	previousGuild       *discordgo.Guild
	selectedChannelNode *tview.TreeNode
	previousChannelNode *tview.TreeNode
	selectedChannel     *discordgo.Channel
	previousChannel     *discordgo.Channel

	jsEngine scripting.Engine

	commandMode bool
	commandView *CommandView
	commands    []commands.Command

	userActive      bool
	userActiveTimer *time.Timer

	doRestart chan bool
}

//NewWindow constructs the whole application window and also registers all
//necessary handlers and functions. If this function returns an error, we can't
//start the application.
func NewWindow(doRestart chan bool, app *tview.Application, session *discordgo.Session, readyEvent *discordgo.Ready) (*Window, error) {
	window := &Window{
		doRestart:       doRestart,
		session:         session,
		app:             app,
		jsEngine:        js.New(),
		userActiveTimer: time.NewTimer(userInactiveTime),
	}

	go func() {
		for {
			<-window.userActiveTimer.C
			window.userActive = false
		}
	}()

	window.commandView = NewCommandView(window.ExecuteCommand)
	log.SetOutput(window.commandView)

	window.jsEngine.SetErrorOutput(window.commandView.commandOutput)
	if err := window.jsEngine.LoadScripts(config.GetScriptDirectory()); err != nil {
		return nil, err
	}

	guilds := readyEvent.Guilds

	mentionWindowRootNode := tview.NewTreeNode("")
	mentionWindow := tview.NewTreeView().
		SetVimBindingsEnabled(false).
		SetRoot(mentionWindowRootNode).
		SetTopLevel(1).
		SetCycleSelection(true)
	mentionWindow.SetBorder(true)
	mentionWindow.SetBorderSides(false, true, false, true)

	window.leftArea = tview.NewPages()

	guildPage := tview.NewFlex()
	guildPage.SetDirection(tview.FlexRow)

	channelTree := NewChannelTree(window.session.State)
	window.channelTree = channelTree
	channelTree.SetOnChannelSelect(func(channelID string) {
		channel, cacheError := window.session.State.Channel(channelID)
		if cacheError == nil {
			go func() {
				window.chatView.Lock()
				defer window.chatView.Unlock()
				window.QueueUpdateDrawSynchronized(func() {
					loadError := window.LoadChannel(channel)
					if loadError == nil {
						channelTree.MarkChannelAsLoaded(channelID)
					}
				})
			}()
		}
	})
	window.registerGuildChannelHandler()

	discordutil.SortGuilds(window.session.State.Settings, guilds)
	guildList := NewGuildList(guilds, window)
	guildList.SetOnGuildSelect(func(node *tview.TreeNode, guildID string) {
		if window.selectedGuild != nil && window.selectedGuildNode != nil {
			window.updateServerReadStatus(window.selectedGuild.ID, window.selectedGuildNode, false)
		}

		guild, cacheError := window.session.Guild(guildID)
		if cacheError != nil {
			window.ShowErrorDialog(cacheError.Error())
			return
		}

		// previousGuild and previousGuildNode should be set initially.
		// If going from first guild -> private chat, SwitchToPreviousChannel would crash.
		if window.selectedGuildNode == nil {
			window.previousGuildNode = node
			window.previousGuild = guild
		} else if window.previousGuildNode != window.selectedGuildNode {
			window.previousGuildNode = window.selectedGuildNode
			window.previousGuild = window.selectedGuild
		}

		window.selectedGuildNode = node
		window.selectedGuild = guild

		window.updateServerReadStatus(window.selectedGuild.ID, window.selectedGuildNode, true)

		requestError := session.RequestGuildMembers(guildID, "", 0)
		if requestError != nil {
			fmt.Fprintln(window.commandView, "Error retrieving all guild members.")
		}

		channelLoadError := window.channelTree.LoadGuild(guildID)
		if channelLoadError != nil {
			window.ShowErrorDialog(channelLoadError.Error())
		} else {
			if config.GetConfig().FocusChannelAfterGuildSelection {
				app.SetFocus(window.channelTree)
			}
		}

		userLoadError := window.userList.LoadGuild(guildID)
		if userLoadError != nil {
			window.ShowErrorDialog(userLoadError.Error())
		}

		window.RefreshLayout()
	})

	window.guildList = guildList

	window.registerGuildHandlers()
	window.registerGuildMemberHandlers()

	if config.GetConfig().MouseEnabled {
		switchToPrivateButton := tview.NewButton("Show PMs")
		switchToPrivateButton.SetBorderColor(config.GetTheme().PrimitiveBackgroundColor)
		switchToPrivateButton.SetSelectedFunc(func() {
			window.SwitchToFriendsPage()
		})
		switchToPrivateButtonInsets := tview.NewFlex().SetDirection(tview.FlexColumn)
		switchToPrivateButtonInsets.AddItem(tview.NewBox(), 1, 0, false)
		switchToPrivateButtonInsets.AddItem(switchToPrivateButton, 0, 1, false)
		switchToPrivateButtonInsets.AddItem(tview.NewBox(), 1, 0, false)

		guildPage.AddItem(switchToPrivateButtonInsets, 1, 0, false)
	}
	guildPage.AddItem(guildList, 0, 1, true)
	guildPage.AddItem(channelTree, 0, 2, false)

	window.leftArea.AddPage(guildPageName, guildPage, true, false)

	window.privateList = NewPrivateChatList(window.session.State)
	window.privateList.Load()
	window.registerPrivateChatsHandler()

	if config.GetConfig().MouseEnabled {
		privatePage := tview.NewFlex().SetDirection(tview.FlexRow)
		switchToGuildButton := tview.NewButton("Show Guilds")
		switchToGuildButton.SetBorderColor(config.GetTheme().PrimitiveBackgroundColor)
		switchToGuildButton.SetSelectedFunc(func() {
			window.SwitchToGuildsPage()
		})
		switchToGuildButtonInsets := tview.NewFlex().SetDirection(tview.FlexColumn)
		switchToGuildButtonInsets.AddItem(tview.NewBox(), 1, 0, false)
		switchToGuildButtonInsets.AddItem(switchToGuildButton, 0, 1, false)
		switchToGuildButtonInsets.AddItem(tview.NewBox(), 1, 0, false)
		privatePage.AddItem(switchToGuildButtonInsets, 1, 0, false)
		privatePage.AddItem(window.privateList.GetComponent(), 0, 1, false)

		window.leftArea.AddPage(privatePageName, privatePage, true, false)
	} else {
		window.leftArea.AddPage(privatePageName, window.privateList.GetComponent(), true, false)
	}

	window.privateList.SetOnChannelSelect(func(node *tview.TreeNode, channelID string) {
		channel, stateError := window.session.State.Channel(channelID)
		if stateError != nil {
			window.ShowErrorDialog(fmt.Sprintf("Error loading chat: %s", stateError.Error()))
			return
		}

		go func() {
			window.chatView.Lock()
			defer window.chatView.Unlock()
			window.QueueUpdateDrawSynchronized(func() {
				window.LoadChannel(channel)

				if channel.Type == discordgo.ChannelTypeGroupDM {
					loadError := window.userList.LoadGroup(channel.ID)
					if loadError != nil {
						fmt.Fprintln(window.commandView.commandOutput, "Error loading users for channel.")
					}
				}

				window.RefreshLayout()
			})
		}()
	})

	window.privateList.SetOnFriendSelect(func(userID string) {
		go func() {
			window.chatView.Lock()
			defer window.chatView.Unlock()
			userChannels, _ := window.session.UserChannels()
			for _, userChannel := range userChannels {
				if userChannel.Type == discordgo.ChannelTypeDM &&
					(userChannel.Recipients[0].ID == userID) {
					window.QueueUpdateDrawSynchronized(func() {
						window.loadPrivateChannel(userChannel)
					})
					return
				}
			}

			newChannel, discordError := window.session.UserChannelCreate(userID)
			if discordError == nil {
				messages, discordError := window.session.ChannelMessages(newChannel.ID, 100, "", "", "")
				if discordError == nil {
					for _, message := range messages {
						window.session.State.MessageAdd(message)
					}
				}
				window.QueueUpdateDrawSynchronized(func() {
					window.loadPrivateChannel(newChannel)
				})
			}
		}()
	})

	window.chatArea = tview.NewFlex().
		SetDirection(tview.FlexRow)

	window.chatView = NewChatView(window.session.State, window.session.State.User.ID)
	window.chatView.SetOnMessageAction(func(message *discordgo.Message, event *tcell.EventKey) *tcell.EventKey {
		if shortcuts.QuoteSelectedMessage.Equals(event) {
			window.insertQuoteOfMessage(message)
			return nil
		}

		if shortcuts.ReplySelectedMessage.Equals(event) {
			window.messageInput.SetText("@" + message.Author.Username + "#" + message.Author.Discriminator + " " + window.messageInput.GetText())
			app.SetFocus(window.messageInput.GetPrimitive())
			return nil
		}

		if shortcuts.CopySelectedMessageLink.Equals(event) {
			copyError := clipboard.WriteAll(fmt.Sprintf("<https://discordapp.com/channels/@me/%s/%s>", message.ChannelID, message.ID))
			if copyError != nil {
				window.ShowErrorDialog(fmt.Sprintf("Error copying message link: %s", copyError.Error()))
			}
			return nil
		}

		if shortcuts.DeleteSelectedMessage.Equals(event) {
			if message.Author.ID == window.session.State.User.ID {
				window.askForMessageDeletion(message.ID, true)
			}
			return nil
		}

		if shortcuts.EditSelectedMessage.Equals(event) {
			window.startEditingMessage(message)
			return nil
		}

		if shortcuts.CopySelectedMessage.Equals(event) {
			copyError := clipboard.WriteAll(message.ContentWithMentionsReplaced())
			if copyError != nil {
				window.ShowErrorDialog(fmt.Sprintf("Error copying message: %s", copyError.Error()))
			}
			return nil
		}

		return event
	})
	window.messageContainer = window.chatView.GetPrimitive()

	window.messageInput = NewEditor()
	window.messageInput.internalTextView.SetIndicateOverflow(true)
	window.messageInput.SetOnHeightChangeRequest(func(height int) {
		_, _, _, chatViewHeight := window.chatView.internalTextView.GetRect()
		newHeight := maths.Min(height, chatViewHeight/2)

		window.chatArea.ResizeItem(window.messageInput.GetPrimitive(), newHeight, 0)
	})

	window.messageInput.SetMentionShowHandler(func(namePart string) {
		mentionWindow.GetRoot().ClearChildren()
		window.commandView.commandOutput.Clear()

		window.PopulateMentionWindow(mentionWindow, namePart)
		if !window.ShowMentionWindowChildren(mentionWindow, 10) {
			window.HideMentionWindow(mentionWindow)
		}
	})

	window.messageInput.SetMentionHideHandler(func() {
		window.HideMentionWindow(mentionWindow)
	})

	window.messageInput.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		messageToSend := window.messageInput.GetText()

		if event.Modifiers() == tcell.ModAlt {
			if event.Key() == tcell.KeyUp {
				window.app.SetFocus(window.chatView.internalTextView)
				return nil
			}

			if event.Key() == tcell.KeyDown {
				if window.commandMode {
					window.app.SetFocus(window.commandView.commandOutput)
				} else {
					window.app.SetFocus(window.chatView.internalTextView)
				}
				return nil
			}

			if event.Key() == tcell.KeyRight {
				if window.userList.internalTreeView.IsVisible() {
					window.app.SetFocus(window.userList.internalTreeView)
				} else {
					if window.leftArea.GetCurrentPage() == guildPageName {
						window.app.SetFocus(window.channelTree)
						return nil
					} else if window.leftArea.GetCurrentPage() == privatePageName {
						window.app.SetFocus(window.privateList.internalTreeView)
						return nil
					}
				}
				return nil
			}

			if event.Key() == tcell.KeyLeft {
				if window.leftArea.GetCurrentPage() == guildPageName {
					window.app.SetFocus(window.channelTree)
					return nil
				} else if window.leftArea.GetCurrentPage() == privatePageName {
					window.app.SetFocus(window.privateList.internalTreeView)
					return nil
				}
			}
		}

		if event.Modifiers() == tcell.ModCtrl {
			if event.Key() == tcell.KeyUp {
				window.chatView.internalTextView.ScrollUp()
				return nil
			}

			if event.Key() == tcell.KeyDown {
				window.chatView.internalTextView.ScrollDown()
				return nil
			}
		}

		if event.Key() == tcell.KeyPgUp {
			handler := window.chatView.internalTextView.InputHandler()
			handler(tcell.NewEventKey(tcell.KeyPgUp, 0, tcell.ModNone), nil)
			return nil
		}

		if event.Key() == tcell.KeyPgDn {
			handler := window.chatView.internalTextView.InputHandler()
			handler(tcell.NewEventKey(tcell.KeyPgDn, 0, tcell.ModNone), nil)
			return nil
		}

		if event.Key() == tcell.KeyUp && messageToSend == "" {
			for i := len(window.chatView.data) - 1; i >= 0; i-- {
				message := window.chatView.data[i]
				if message.Author.ID == window.session.State.User.ID {
					window.startEditingMessage(message)
					break
				}
			}

			return nil
		}

		if event.Key() == tcell.KeyEsc {
			window.exitMessageEditMode()
			return nil
		}

		if event.Key() == tcell.KeyCtrlV && window.selectedChannel != nil {
			data, clipError := goclipimg.GetImageFromClipboard()

			if clipError == goclipimg.ErrNoImageInClipboard {
				return event
			}

			if clipError == nil {
				dataChannel := bytes.NewReader(data)
				targetChannel := window.selectedChannel
				currentText := window.prepareMessage(targetChannel, strings.TrimSpace(window.messageInput.GetText()))
				if currentText == "" {
					go window.session.ChannelFileSend(targetChannel.ID, "img.png", dataChannel)
				} else {
					go window.session.ChannelFileSendWithMessage(targetChannel.ID, currentText, "img.png", dataChannel)
					window.messageInput.SetText("")
				}
			} else {
				window.ShowErrorDialog(fmt.Sprintf("Error pasting image: %s", clipError.Error()))
			}

			return nil
		}

		if shortcuts.AddNewLineInCodeBlock.Equals(event) && window.IsCursorInsideCodeBlock() {
			window.insertNewLineAtCursor()
			return nil
		} else if shortcuts.SendMessage.Equals(event) {
			if window.selectedChannel != nil {
				window.TrySendMessage(window.selectedChannel, messageToSend)
			}
			return nil
		}

		return event
	})

	messageInputChan := make(chan *discordgo.Message, 200)
	messageDeleteChan := make(chan *discordgo.Message, 50)
	messageEditChan := make(chan *discordgo.Message, 50)
	messageBulkDeleteChan := make(chan *discordgo.MessageDeleteBulk, 50)

	window.registerMessageEventHandler(messageInputChan, messageEditChan, messageDeleteChan, messageBulkDeleteChan)
	window.startMessageHandlerRoutines(messageInputChan, messageEditChan, messageDeleteChan, messageBulkDeleteChan)

	window.userList = NewUserTree(window.session.State)

	if config.GetConfig().OnTypeInListBehaviour == config.SearchOnTypeInList {
		guildList.SetSearchOnTypeEnabled(true)
		channelTree.SetSearchOnTypeEnabled(true)
		window.userList.internalTreeView.SetSearchOnTypeEnabled(true)
		window.privateList.internalTreeView.SetSearchOnTypeEnabled(true)
	} else if config.GetConfig().OnTypeInListBehaviour == config.FocusMessageInputOnTypeInList {
		guildList.SetInputCapture(tviewutil.CreateFocusTextViewOnTypeInputHandler(
			window.app, window.messageInput.internalTextView))
		channelTree.SetInputCapture(tviewutil.CreateFocusTextViewOnTypeInputHandler(
			window.app, window.messageInput.internalTextView))
		window.userList.SetInputCapture(tviewutil.CreateFocusTextViewOnTypeInputHandler(
			window.app, window.messageInput.internalTextView))
		window.privateList.SetInputCapture(tviewutil.CreateFocusTextViewOnTypeInputHandler(
			window.app, window.messageInput.internalTextView))
		window.chatView.internalTextView.SetInputCapture(tviewutil.CreateFocusTextViewOnTypeInputHandler(
			window.app, window.messageInput.internalTextView))
	}

	//Guild Container arrow key navigation. Please end my life.
	oldGuildListHandler := guildList.GetInputCapture()
	newGuildHandler := func(event *tcell.EventKey) *tcell.EventKey {
		if event.Modifiers() == tcell.ModAlt {
			if event.Key() == tcell.KeyDown || event.Key() == tcell.KeyUp {
				window.app.SetFocus(window.channelTree)
				return nil
			}

			if event.Key() == tcell.KeyLeft {
				if window.userList.internalTreeView.IsVisible() {
					window.app.SetFocus(window.userList.internalTreeView)
				} else {
					window.app.SetFocus(window.chatView.internalTextView)
				}
				return nil
			}

			if event.Key() == tcell.KeyRight {
				window.app.SetFocus(window.chatView.internalTextView)
				return nil
			}
		}

		return event
	}

	if oldGuildListHandler == nil {
		guildList.SetInputCapture(newGuildHandler)
	} else {
		guildList.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			handledEvent := newGuildHandler(event)
			if handledEvent != nil {
				return oldGuildListHandler(event)
			}

			return event
		})
	}

	//Channel Container arrow key navigation. Please end my life.
	oldChannelListHandler := channelTree.GetInputCapture()
	newChannelListHandler := func(event *tcell.EventKey) *tcell.EventKey {
		if event.Modifiers() == tcell.ModAlt {
			if event.Key() == tcell.KeyDown || event.Key() == tcell.KeyUp {
				window.app.SetFocus(window.guildList)
				return nil
			}

			if event.Key() == tcell.KeyLeft {
				if window.userList.internalTreeView.IsVisible() {
					window.app.SetFocus(window.userList.internalTreeView)
				} else {
					if window.commandMode {
						window.app.SetFocus(window.commandView.commandOutput)
					} else {
						window.app.SetFocus(window.messageInput.GetPrimitive())
					}
				}
				return nil
			}

			if event.Key() == tcell.KeyRight {
				if window.commandMode {
					window.app.SetFocus(window.commandView.commandOutput)
				} else {
					window.app.SetFocus(window.messageInput.GetPrimitive())
				}
				return nil
			}
		}

		return event
	}

	if oldChannelListHandler == nil {
		channelTree.SetInputCapture(newChannelListHandler)
	} else {
		channelTree.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			handledEvent := newChannelListHandler(event)
			if handledEvent != nil {
				return oldChannelListHandler(event)
			}

			return event
		})
	}

	//Chatview arrow key navigation. Please end my life.
	oldChatViewHandler := window.chatView.internalTextView.GetInputCapture()
	newChatViewHandler := func(event *tcell.EventKey) *tcell.EventKey {
		if event.Modifiers() == tcell.ModAlt {
			if event.Key() == tcell.KeyDown {
				window.app.SetFocus(window.messageInput.GetPrimitive())
				return nil
			}

			if event.Key() == tcell.KeyUp {
				if window.commandMode {
					window.app.SetFocus(window.commandView.commandInput.internalTextView)
				} else {
					window.app.SetFocus(window.messageInput.GetPrimitive())
				}
				return nil
			}

			if event.Key() == tcell.KeyLeft {
				if window.leftArea.GetCurrentPage() == guildPageName {
					window.app.SetFocus(window.guildList)
					return nil
				} else if window.leftArea.GetCurrentPage() == guildPageName {
					window.app.SetFocus(window.privateList.internalTreeView)
					return nil
				}
			}

			if event.Key() == tcell.KeyRight {
				if window.userList.internalTreeView.IsVisible() {
					window.app.SetFocus(window.userList.internalTreeView)
				} else {
					if window.leftArea.GetCurrentPage() == guildPageName {
						window.app.SetFocus(window.guildList)
						return nil
					} else if window.leftArea.GetCurrentPage() == guildPageName {
						window.app.SetFocus(window.privateList.internalTreeView)
						return nil
					}
				}
				return nil
			}
		}

		return event
	}

	if oldChatViewHandler == nil {
		window.chatView.internalTextView.SetInputCapture(newChatViewHandler)
	} else {
		window.chatView.internalTextView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			handledEvent := newChatViewHandler(event)
			if handledEvent != nil {
				return oldChatViewHandler(event)
			}

			return event
		})
	}

	//User Container arrow key navigation. Please end my life.
	oldUserListHandler := window.userList.internalTreeView.GetInputCapture()
	newUserListHandler := func(event *tcell.EventKey) *tcell.EventKey {
		if event.Modifiers() == tcell.ModAlt {
			if event.Key() == tcell.KeyRight {
				if window.leftArea.GetCurrentPage() == guildPageName {
					window.app.SetFocus(window.guildList)
					return nil
				} else if window.leftArea.GetCurrentPage() == guildPageName {
					window.app.SetFocus(window.privateList.internalTreeView)
					return nil
				}
				return nil
			}
			if event.Key() == tcell.KeyLeft {
				window.app.SetFocus(window.chatView.GetPrimitive())
				return nil
			}
		}

		return event
	}

	if oldUserListHandler == nil {
		window.userList.internalTreeView.SetInputCapture(newUserListHandler)
	} else {
		window.userList.internalTreeView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			handledEvent := newUserListHandler(event)
			if handledEvent != nil {
				return oldUserListHandler(event)
			}

			return event
		})
	}

	//Private Container arrow key navigation. Please end my life.
	oldPrivateListHandler := window.privateList.internalTreeView.GetInputCapture()
	newPrivateListHandler := func(event *tcell.EventKey) *tcell.EventKey {
		if event.Modifiers() == tcell.ModAlt {
			if event.Key() == tcell.KeyLeft {
				if window.userList.internalTreeView.IsVisible() {
					window.app.SetFocus(window.userList.internalTreeView)
				} else {
					window.app.SetFocus(window.chatView.internalTextView)
				}
				return nil
			}

			if event.Key() == tcell.KeyRight {
				window.app.SetFocus(window.chatView.internalTextView)
				return nil
			}
		}

		return event
	}

	if oldPrivateListHandler == nil {
		window.privateList.internalTreeView.SetInputCapture(newPrivateListHandler)
	} else {
		window.privateList.internalTreeView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			handledEvent := newPrivateListHandler(event)
			if handledEvent != nil {
				return oldPrivateListHandler(event)
			}

			return event
		})
	}

	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.MessageAck) {
		if readstate.UpdateReadLocal(event.ChannelID, event.MessageID) {
			channel, stateError := s.State.Channel(event.ChannelID)
			if stateError == nil && event.MessageID == channel.LastMessageID {
				if channel.GuildID == "" {
					window.privateList.MarkChannelAsRead(channel.ID)
				} else {
					if window.selectedGuild != nil && channel.GuildID == window.selectedGuild.ID {
						window.channelTree.MarkChannelAsRead(channel.ID)
					} else {
						for _, guildNode := range window.guildList.GetRoot().GetChildren() {
							if guildNode.GetReference() == channel.GuildID {
								window.updateServerReadStatus(channel.GuildID, guildNode, false)
								break
							}
						}
					}
				}
			}
		}
	})

	window.commandView.SetInputCaptureForInput(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Modifiers() == tcell.ModAlt {
			if event.Key() == tcell.KeyUp {
				window.app.SetFocus(window.commandView.commandOutput)
			} else if event.Key() == tcell.KeyDown {
				window.app.SetFocus(window.chatView.GetPrimitive())
			} else if event.Key() == tcell.KeyRight {
				if window.userList.internalTreeView.IsVisible() {
					window.app.SetFocus(window.userList.internalTreeView)
				} else {
					window.app.SetFocus(window.channelTree)
				}
			} else if event.Key() == tcell.KeyLeft {
				window.app.SetFocus(window.channelTree)
			} else {
				return event
			}

			return nil
		}

		return event
	})

	window.commandView.SetInputCaptureForOutput(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Modifiers() == tcell.ModAlt {
			if event.Key() == tcell.KeyUp {
				window.app.SetFocus(window.messageInput.GetPrimitive())
			} else if event.Key() == tcell.KeyDown {
				window.app.SetFocus(window.commandView.commandInput.GetPrimitive())
			} else if event.Key() == tcell.KeyRight {
				if window.userList.internalTreeView.IsVisible() {
					window.app.SetFocus(window.userList.internalTreeView)
				} else {
					window.app.SetFocus(window.channelTree)
				}
			} else if event.Key() == tcell.KeyLeft {
				window.app.SetFocus(window.channelTree)
			} else {
				return event
			}

			return nil
		}

		return event
	})

	window.middleContainer = tview.NewFlex().
		SetDirection(tview.FlexColumn)

	window.rootContainer = tview.NewFlex().
		SetDirection(tview.FlexRow)
	window.rootContainer.SetTitleAlign(tview.AlignCenter)
	window.rootContainer.AddItem(window.middleContainer, 0, 1, false)

	window.dialogReplacement = tview.NewFlex().
		SetDirection(tview.FlexRow)

	window.dialogTextView = tview.NewTextView()
	window.dialogReplacement.AddItem(window.dialogTextView, 0, 1, false)

	window.dialogButtonBar = tview.NewFlex().
		SetDirection(tview.FlexColumn)

	window.dialogReplacement.AddItem(window.dialogButtonBar, 1, 0, false)
	window.dialogReplacement.SetVisible(false)

	window.rootContainer.AddItem(window.dialogReplacement, 2, 0, false)

	app.SetRoot(window.rootContainer, true)
	window.currentContainer = window.rootContainer
	app.SetInputCapture(window.handleGlobalShortcuts)

	conf := config.GetConfig()

	if conf.UseFixedLayout {
		window.middleContainer.AddItem(window.leftArea, conf.FixedSizeLeft, 0, true)
		window.middleContainer.AddItem(window.chatArea, 0, 1, false)
		window.middleContainer.AddItem(window.userList.internalTreeView, conf.FixedSizeRight, 0, false)
	} else {
		window.middleContainer.AddItem(window.leftArea, 0, 7, true)
		window.middleContainer.AddItem(window.chatArea, 0, 20, false)
		window.middleContainer.AddItem(window.userList.internalTreeView, 0, 6, false)
	}

	mentionWindow.SetVisible(false)
	mentionWindow.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch key := event.Key(); key {
		case tcell.KeyRune, tcell.KeyDelete, tcell.KeyBackspace, tcell.KeyBackspace2, tcell.KeyLeft, tcell.KeyRight, tcell.KeyCtrlA, tcell.KeyCtrlV:
			window.messageInput.internalTextView.GetInputCapture()(event)
			return nil
		}
		return event
	})

	// Invoked when enter is pressed
	mentionWindow.SetSelectedFunc(func(node *tview.TreeNode) {
		beginIndex, endIndex := window.messageInput.GetCurrentMentionIndices()
		data, ok := node.GetReference().(string)
		oldText := window.messageInput.GetText()
		if ok {
			left := oldText[:beginIndex] + strings.TrimSpace(data) + " "
			right := oldText[endIndex+1:]
			var text string
			if len(right) == 0 {
				text = left + " "
			} else {
				text = left + right
			}
			window.messageInput.SetText(text)
			window.messageInput.MoveCursorToIndex(text, len(left))
		} else {
			role, ok := node.GetReference().(*discordgo.Role)
			if ok {
				left := "<@&" + strings.TrimSpace(role.ID) + "> "
				right := oldText[endIndex+1:]
				var text string
				if len(right) == 0 {
					text = left + " "
				} else {
					text = left + right
				}
				window.messageInput.SetText(text)
				window.messageInput.MoveCursorToIndex(text, len(left))
			}
		}
		window.messageInput.mentionHideHandler()
	})

	window.chatArea.AddItem(window.messageContainer, 0, 1, false)
	window.chatArea.AddItem(mentionWindow, 2, 2, true)
	window.chatArea.AddItem(window.messageInput.GetPrimitive(), window.messageInput.GetRequestedHeight(), 0, false)

	window.commandView.commandOutput.SetVisible(false)
	window.commandView.commandInput.internalTextView.SetVisible(false)

	window.chatArea.AddItem(window.commandView.commandOutput, 0, 1, false)
	window.chatArea.AddItem(window.commandView.commandInput.internalTextView, 3, 0, false)

	window.SwitchToGuildsPage()

	app.SetFocus(guildList)

	window.registerMouseFocusListeners()

	return window, nil
}

func (window *Window) loadPrivateChannel(channel *discordgo.Channel) {
	window.LoadChannel(channel)
	window.RefreshLayout()
}

func (window *Window) insertNewLineAtCursor() {
	left := window.messageInput.internalTextView.GetRegionText("left")
	right := window.messageInput.internalTextView.GetRegionText("right")
	selection := window.messageInput.internalTextView.GetRegionText("selection")
	window.messageInput.InsertCharacter([]rune(left), []rune(right), []rune(selection), '\n')
	window.app.QueueUpdateDraw(func() {
		window.messageInput.triggerHeightRequestIfNeccessary()
		window.messageInput.internalTextView.ScrollToHighlight()
	})
}

func (window *Window) IsCursorInsideCodeBlock() bool {
	left := window.messageInput.internalTextView.GetRegionText("left")
	leftSplit := strings.Split(left, "```")
	return len(leftSplit)%2 == 0
}

func (window *Window) insertQuoteOfMessage(message *discordgo.Message) {
	messageTime, parseError := message.Timestamp.Parse()
	if parseError != nil {
		return
	}

	// All quotes should be UTC.
	messageTimeUTC := messageTime.UTC()

	currentContent := strings.TrimSpace(window.messageInput.GetText())
	username := message.Author.Username
	if message.GuildID != "" {
		guild, stateError := window.session.State.Guild(message.GuildID)
		if stateError == nil {
			member, stateError := window.session.State.Member(guild.ID, message.Author.ID)
			if stateError == nil && member.Nick != "" {
				username = member.Nick
			}
		}
	}

	quotedMessage := strings.ReplaceAll(message.ContentWithMentionsReplaced(), "\n", "\n> ")
	quotedMessage = fmt.Sprintf("> **%s** %s UTC:\n> %s\n", username, times.TimeToString(&messageTimeUTC), quotedMessage)
	if currentContent != "" {
		quotedMessage = quotedMessage + currentContent
	}
	window.messageInput.SetText(quotedMessage)
	window.app.SetFocus(window.messageInput.GetPrimitive())
}

func (window *Window) TrySendMessage(targetChannel *discordgo.Channel, message string) {
	if targetChannel == nil {
		return
	}

	if len(message) == 0 {
		if window.editingMessageID != nil {
			msgIDCopy := *window.editingMessageID
			window.askForMessageDeletion(msgIDCopy, true)
		}
		return
	}

	message = strings.TrimSpace(message)
	if len(message) == 0 {
		window.app.QueueUpdateDraw(func() {
			window.messageInput.SetText("")
		})
		return
	}

	message = window.prepareMessage(targetChannel, message)
	if len(message) > 2000 {
		window.app.QueueUpdateDraw(func() {
			window.ShowErrorDialog("Messages must be 2000 characters or less to send")
		})
		return
	}

	if window.editingMessageID != nil {
		window.editMessage(targetChannel.ID, *window.editingMessageID, message)
		return
	}

	go window.sendMessage(targetChannel.ID, message)
}

func (window *Window) sendMessage(targetChannelID, message string) {
	window.app.QueueUpdateDraw(func() {
		window.messageInput.SetText("")
		window.chatView.internalTextView.ScrollToEnd()
	})
	_, sendError := window.session.ChannelMessageSend(targetChannelID, message)
	if sendError != nil {
		window.app.QueueUpdateDraw(func() {
			retry := "Retry sending"
			edit := "Edit"
			cancel := "Cancel"
			window.ShowDialog(config.GetTheme().ErrorColor,
				fmt.Sprintf("Error sending message: %s.\n\nWhat do you want to do?", sendError),
				func(button string) {
					switch button {
					case retry:
						go window.sendMessage(targetChannelID, message)
					case edit:
						window.messageInput.SetText(message)
					}
				}, retry, edit, cancel)
		})
	}
}

func (window *Window) HideMentionWindow(mentionWindow *tview.TreeView) {
	mentionWindow.SetVisible(false)
	window.app.SetFocus(window.messageInput.internalTextView)
}

func (window *Window) ShowMentionWindowChildren(mentionWindow *tview.TreeView, maxChildren int) bool {
	children := mentionWindow.GetRoot().GetChildren()
	if children == nil {
		return false
	}
	numChildren := maths.Min(len(children), 10)
	window.chatArea.ResizeItem(mentionWindow, numChildren, 0)
	if numChildren > 0 {
		mentionWindow.SetCurrentNode(children[0])
	}
	mentionWindow.SetVisible(true)
	window.app.SetFocus(mentionWindow)
	return true
}

func (window *Window) PopulateMentionWindow(mentionWindow *tview.TreeView, namePart string) {
	if window.selectedChannel == nil {
		return
	}

	if window.selectedChannel.GuildID != "" {
		window.PopulateMentionWindowFromCurrentGuild(mentionWindow, namePart)
	} else {
		window.PopulateMentionWindowFromCurrentChannel(mentionWindow, namePart)
	}
}

func (window *Window) PopulateMentionWindowFromCurrentGuild(mentionWindow *tview.TreeView, namePart string) {
	guild, discordError := window.session.State.Guild(window.selectedChannel.GuildID)
	if discordError != nil {
		return
	}

	nameMap := make(map[string]string)
	var memberNames []string
	for _, user := range guild.Members {
		userName := user.User.Username + "#" + user.User.Discriminator
		if len(user.Nick) > 0 {
			combined := "\t" + userName + " | " + user.Nick
			nameMap[userName] = combined
			nameMap[user.Nick] = combined
			nameMap[combined] = userName
			memberNames = append(memberNames, userName, user.Nick)
		} else {
			memberNames = append(memberNames, userName)
		}
	}

	roleMap := make(map[string]*discordgo.Role)
	for _, role := range guild.Roles {
		roleMap[role.Name] = role
		memberNames = append(memberNames, role.Name)
	}

	searchResults := fuzzy.ScoreSearch(namePart, memberNames)
	sortedResults := fuzzy.SortSearchResults(searchResults)

	userWithNickSet := make(map[string]struct{})
	for _, result := range sortedResults {
		// Check if result was a role.
		if role, ok := roleMap[result.Key]; ok {
			window.addNodeToMentionWindow(mentionWindow, role.Name, role)
			continue
		}

		var displayName string = result.Key
		var userMentionReference string = result.Key
		if combinedUserAndNickName, ok := nameMap[result.Key]; ok {
			// If the combined string has been added, skip this entry.
			if _, containsStr := userWithNickSet[combinedUserAndNickName]; containsStr {
				continue
			}
			userWithNickSet[combinedUserAndNickName] = struct{}{}
			displayName = combinedUserAndNickName
			userMentionReference = nameMap[combinedUserAndNickName]
		}
		window.addNodeToMentionWindow(mentionWindow, displayName, userMentionReference)
	}

}

func (window *Window) addNodeToMentionWindow(mentionWindow *tview.TreeView, name string, reference interface{}) {
	userNode := tview.NewTreeNode(name)
	userNode.SetReference(reference)
	mentionWindow.GetRoot().AddChild(userNode)
}

func (window *Window) PopulateMentionWindowFromCurrentChannel(mentionWindow *tview.TreeView, namePart string) {
	memberNames := make([]string, 0, len(window.selectedChannel.Recipients))
	for _, user := range window.selectedChannel.Recipients {
		userName := user.Username + "#" + user.Discriminator
		memberNames = append(memberNames, userName)
	}

	searchResults := fuzzy.ScoreSearch(namePart, memberNames)
	sortedResults := fuzzy.SortSearchResults(searchResults)

	for _, result := range sortedResults {
		userName := result.Key
		userNodeText := "\t" + userName
		userNode := tview.NewTreeNode(userNodeText)
		userNode.SetReference(userName)
		mentionWindow.GetRoot().AddChild(userNode)
	}

}

func (window *Window) updateServerReadStatus(guildID string, guildNode *tview.TreeNode, isSelected bool) {
	if isSelected {
		guildNode.SetColor(tview.Styles.ContrastBackgroundColor)
	} else {
		if !readstate.HasGuildBeenRead(guildID) {
			guildNode.SetColor(config.GetTheme().AttentionColor)
		} else {
			guildNode.SetColor(tview.Styles.PrimaryTextColor)
		}
	}
}

// prepareMessage prepares a message for being sent to the discord API.
// This will do all necessary escaping and resolving of channel-mentions,
// user-mentions, emojis and the likes.
//
// The input is expected to be a string without sorrounding whitespace.
func (window *Window) prepareMessage(targetChannel *discordgo.Channel, inputText string) string {
	message := codeBlockRegex.ReplaceAllStringFunc(inputText, func(input string) string {
		return strings.ReplaceAll(input, ":", "\\:")
	})

	message = window.jsEngine.OnMessageSend(message)

	//Replace formatter characters and replace emoji codes.
	message = discordemojimap.Replace(message)

	if targetChannel.GuildID != "" {
		channelGuild, discordError := window.session.State.Guild(targetChannel.GuildID)
		if discordError == nil {
			//Those could be optimized by searching the string for patterns.
			for _, channel := range channelGuild.Channels {
				if channel.Type == discordgo.ChannelTypeGuildText {
					message = strings.ReplaceAll(message, "#"+channel.Name, "<#"+channel.ID+">")
				}
			}

			message = window.replaceCustomEmojiSequences(channelGuild, message)
		}
	} else {
		message = window.replaceCustomEmojiSequences(nil, message)
	}

	message = strings.Replace(message, "\\:", ":", -1)

	if targetChannel.GuildID == "" {
		for _, user := range targetChannel.Recipients {
			message = strings.ReplaceAll(message, "@"+user.Username+"#"+user.Discriminator, "<@"+user.ID+">")
		}
	} else {
		members, discordError := window.session.State.Members(targetChannel.GuildID)
		if discordError == nil {
			for _, member := range members {
				message = strings.ReplaceAll(message, "@"+member.User.Username+"#"+member.User.Discriminator, "<@"+member.User.ID+">")
			}
		}
	}

	return message
}

// replaceCustomEmojiSequences replaces all emoji codes for non-unicode
// emojis, e.g. discord custom emojis. the search is lowercase and doesn't
// differentiate between emojis with the same name. Instead it goes by whatever
// it finds first. For private channels, the channelGuild may be nil.
func (window *Window) replaceCustomEmojiSequences(channelGuild *discordgo.Guild, message string) string {
	//Simple handling for nitro, since nitro users can pretty much send anything.
	if window.session.State.User.PremiumType == discordgo.UserPremiumTypeNitroClassic ||
		window.session.State.User.PremiumType == discordgo.UserPremiumTypeNitro {
		return emojiRegex.ReplaceAllStringFunc(message, func(match string) string {
			firstDoubleColon := strings.IndexRune(match, ':')
			emojiSequence := strings.ToLower(strings.TrimPrefix(match[firstDoubleColon+1:len(match)-1], "!"))
			for _, guild := range window.session.State.Guilds {
				for _, emoji := range guild.Emojis {
					if strings.ToLower(emoji.Name) == emojiSequence {
						if emoji.Animated {
							return match[:firstDoubleColon] + "<a:" + emoji.Name + ":" + emoji.ID + ">"
						} else {
							return match[:firstDoubleColon] + "<:" + emoji.Name + ":" + emoji.ID + ">"
						}
					}
				}
			}

			return match
		})
	}

	return emojiRegex.ReplaceAllStringFunc(message, func(match string) string {
		firstDoubleColon := strings.IndexRune(match, ':')
		emojiSequence := strings.ToLower(strings.TrimPrefix(match[firstDoubleColon+1:len(match)-1], "!"))

		//Local guild emojis take priority
		if channelGuild != nil {
			emojiResult := window.findMatchInGuild(channelGuild, true, emojiSequence)
			if emojiResult != "" {
				return match[:firstDoubleColon] + emojiResult
			}
		}

		//Check for global emotes
		for _, guild := range window.session.State.Guilds {
			emojiResult := window.findMatchInGuild(guild, false, emojiSequence)
			if emojiResult != "" {
				return match[:firstDoubleColon] + emojiResult
			}
		}

		return match
	})
}

// findMatchInGuild searches for a fitting emoji. Fitting means the correct name
// (caseinsensitive), not animated and the correct permissions. If the result
// is an empty string, it means no result was found.
func (window *Window) findMatchInGuild(guild *discordgo.Guild, omitGWCheck bool, emojiSequence string) string {
	for _, emoji := range guild.Emojis {
		if emoji.Animated {
			continue
		}

		if strings.ToLower(emoji.Name) == emojiSequence && (omitGWCheck || strings.HasPrefix(emoji.Name, "GW")) {
			if len(emoji.Roles) != 0 {
				selfMember, cacheError := window.session.State.Member(guild.ID, window.session.State.User.ID)
				if cacheError != nil {
					selfMember, discordError := window.session.GuildMember(guild.ID, window.session.State.User.ID)
					if discordError != nil {
						log.Println(discordError)
						continue
					}

					window.session.State.MemberAdd(selfMember)
				}

				if selfMember != nil {
					for _, emojiRole := range emoji.Roles {
						for _, selfRole := range selfMember.Roles {
							if selfRole == emojiRole {
								return "<:" + emoji.Name + ":" + emoji.ID + ">"
							}
						}
					}
				}
			}

			return "<:" + emoji.Name + ":" + emoji.ID + ">"
		}
	}

	return ""
}

// ShowDialog shows a dialog at the bottom of the window. It doesn't surrender
// its focus and requires action before allowing the user to proceed. The
// buttons are handled depending on their text.
func (window *Window) ShowDialog(color tcell.Color, text string, buttonHandler func(button string), buttons ...string) {
	window.dialogButtonBar.RemoveAllItems()

	if len(buttons) == 0 {
		return
	}

	previousFocus := window.app.GetFocus()

	buttonWidgets := make([]*tview.Button, 0)
	for index, button := range buttons {
		newButton := tview.NewButton(button)
		newButton.SetSelectedFunc(func() {
			buttonHandler(newButton.GetLabel())
			window.dialogReplacement.SetVisible(false)
			window.app.SetFocus(previousFocus)
		})
		buttonWidgets = append(buttonWidgets, newButton)

		indexCopy := index
		newButton.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			if event.Key() == tcell.KeyRight {
				if len(buttonWidgets) <= indexCopy+1 {
					window.app.SetFocus(buttonWidgets[0])
				} else {
					window.app.SetFocus(buttonWidgets[indexCopy+1])
				}
				return nil
			}

			if event.Key() == tcell.KeyLeft {
				if indexCopy == 0 {
					window.app.SetFocus(buttonWidgets[len(buttonWidgets)-1])
				} else {
					window.app.SetFocus(buttonWidgets[indexCopy-1])
				}
				return nil
			}

			return event
		})

		window.dialogButtonBar.AddItem(newButton, len(button)+2, 0, false)
		window.dialogButtonBar.AddItem(tview.NewBox(), 1, 0, false)
	}
	window.dialogButtonBar.AddItem(tview.NewBox(), 0, 1, false)

	window.dialogTextView.SetText(text)
	window.dialogTextView.SetBackgroundColor(color)
	window.dialogReplacement.SetVisible(true)
	window.app.SetFocus(buttonWidgets[0])

	_, _, width, _ := window.rootContainer.GetRect()
	height := tviewutil.CalculateNeccessaryHeight(width, window.dialogTextView.GetText(true))
	window.rootContainer.ResizeItem(window.dialogReplacement, height+2, 0)
}

func (window *Window) registerMouseFocusListeners() {
	window.chatView.internalTextView.SetMouseHandler(func(event *tcell.EventMouse) bool {
		if event.Buttons() == tcell.Button1 {
			window.app.SetFocus(window.chatView.internalTextView)
		} else if event.Buttons() == tcell.WheelDown {
			window.chatView.internalTextView.ScrollDown()
		} else if event.Buttons() == tcell.WheelUp {
			window.chatView.internalTextView.ScrollUp()
		} else {
			return false
		}

		return true
	})

	window.guildList.SetMouseHandler(func(event *tcell.EventMouse) bool {
		if event.Buttons() == tcell.Button1 {
			window.app.SetFocus(window.guildList)
			return true
		}

		return false
	})
	window.channelTree.SetMouseHandler(func(event *tcell.EventMouse) bool {
		if event.Buttons() == tcell.Button1 {
			window.app.SetFocus(window.channelTree)

			return true
		}

		return false
	})

	window.userList.internalTreeView.SetMouseHandler(func(event *tcell.EventMouse) bool {
		if event.Buttons() == tcell.Button1 {
			window.app.SetFocus(window.userList.internalTreeView)

			return true
		}

		return false
	})

	window.privateList.internalTreeView.SetMouseHandler(func(event *tcell.EventMouse) bool {
		if event.Buttons() == tcell.Button1 {
			window.app.SetFocus(window.privateList.internalTreeView)

			return true
		}

		return false
	})

	window.messageInput.internalTextView.SetMouseHandler(func(event *tcell.EventMouse) bool {
		if event.Buttons() == tcell.Button1 {
			window.app.SetFocus(window.messageInput.internalTextView)

			return true
		}

		return false
	})

	window.commandView.commandInput.internalTextView.SetMouseHandler(func(event *tcell.EventMouse) bool {
		if event.Buttons() == tcell.Button1 {
			window.app.SetFocus(window.commandView.commandInput.internalTextView)

			return true
		}

		return false
	})

	window.commandView.commandOutput.SetMouseHandler(func(event *tcell.EventMouse) bool {
		if event.Buttons() == tcell.Button1 {
			window.app.SetFocus(window.commandView.commandOutput)
		} else if event.Buttons() == tcell.WheelDown {
			window.commandView.commandOutput.ScrollDown()
		} else if event.Buttons() == tcell.WheelUp {
			window.commandView.commandOutput.ScrollUp()
		} else {
			return false
		}

		return true
	})
}

func (window *Window) registerMessageEventHandler(input, edit, delete chan *discordgo.Message, bulkDelete chan *discordgo.MessageDeleteBulk) {
	window.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		input <- m.Message
	})
	window.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageDeleteBulk) {
		bulkDelete <- m
	})

	window.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageDelete) {
		delete <- m.Message
	})

	window.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageUpdate) {
		//Ignore just-embed edits
		if m.Content != "" {
			edit <- m.Message
		}
	})
}

func (window *Window) QueueUpdateDrawSynchronized(runnable func()) {
	blocker := make(chan bool, 1)
	window.app.QueueUpdateDraw(func() {
		runnable()
		blocker <- true
	})
	<-blocker
	close(blocker)
}

// startMessageHandlerRoutines registers the handlers for certain message
// events. It updates the cache and the UI if necessary.
func (window *Window) startMessageHandlerRoutines(input, edit, delete chan *discordgo.Message, bulkDelete chan *discordgo.MessageDeleteBulk) {
	go func() {
		for tempMessage := range input {
			message := tempMessage
			window.session.State.MessageAdd(message)

			channel, stateError := window.session.State.Channel(message.ChannelID)
			if stateError != nil {
				continue
			}

			window.chatView.Lock()
			if window.selectedChannel != nil && message.ChannelID == window.selectedChannel.ID {
				if message.Author.ID != window.session.State.User.ID {
					readstate.UpdateReadBuffered(window.session, channel, message.ID)
				}

				window.QueueUpdateDrawSynchronized(func() {
					window.chatView.AddMessage(message)
				})
			}
			window.chatView.Unlock()

			if channel.Type == discordgo.ChannelTypeGuildText && (window.selectedGuild == nil ||
				window.selectedGuild.ID != channel.GuildID) {
				for _, guildNode := range window.guildList.GetRoot().GetChildren() {
					if guildNode.GetReference() == channel.GuildID {
						window.app.QueueUpdateDraw(func() {
							window.updateServerReadStatus(channel.GuildID, guildNode, false)
						})
						break
					}
				}
			}

			// TODO,HACK.FIXME Since the cache is inconsistent, I have to
			// update it myself. This should be moved over into the
			// discordgo code ASAP.
			channel.LastMessageID = message.ID

			if channel.Type == discordgo.ChannelTypeDM || channel.Type == discordgo.ChannelTypeGroupDM {
				//Avoid unnecessary drawing if the updates wouldn't be visible either way.
				//FIXME Useful to use locking here?
				if window.leftArea.GetCurrentPage() == privatePageName {
					window.app.QueueUpdateDraw(func() {
						window.privateList.ReorderChannelList()
					})
				} else {
					window.privateList.ReorderChannelList()
				}
			}

			if message.Author.ID == window.session.State.User.ID {
				readstate.UpdateReadLocal(message.ChannelID, message.ID)
				continue
			}

			if window.selectedChannel == nil || message.ChannelID != window.selectedChannel.ID {
				mentionsCurrentUser := discordutil.MentionsCurrentUserExplicitly(window.session.State, message)
				if !window.userActive && config.GetConfig().DesktopNotifications {
					if mentionsCurrentUser ||
						//Always show notification for private messages
						channel.Type == discordgo.ChannelTypeDM || channel.Type == discordgo.ChannelTypeGroupDM {

						var notificationLocation string
						if channel.Type == discordgo.ChannelTypeDM {
							notificationLocation = message.Author.Username
						} else if channel.Type == discordgo.ChannelTypeGroupDM {
							notificationLocation = channel.Name
							if notificationLocation == "" {
								for index, recipient := range channel.Recipients {
									if index == 0 {
										notificationLocation = recipient.Username
									} else {
										notificationLocation = fmt.Sprintf("%s, %s", notificationLocation, recipient.Username)
									}
								}
							}

							notificationLocation = message.Author.Username + " - " + notificationLocation
						} else if channel.Type == discordgo.ChannelTypeGuildText {
							guild, cacheError := window.session.State.Guild(message.GuildID)
							if guild != nil && cacheError == nil {
								notificationLocation = fmt.Sprintf("%s - %s - %s", guild.Name, channel.Name, message.Author.Username)
							} else {
								notificationLocation = fmt.Sprintf("%s - %s", message.Author.Username, channel.Name)
							}
						}

						notifyError := beeep.Notify("Cordless - "+notificationLocation, message.ContentWithMentionsReplaced(), "assets/information.png")
						if notifyError != nil {
							log.Printf("["+tviewutil.ColorToHex(config.GetTheme().ErrorColor)+"]Error sending notification:\n\t[%s]%s\n", tviewutil.ColorToHex(config.GetTheme().ErrorColor), notifyError)
						}
					}
				}

				if channel.Type == discordgo.ChannelTypeDM || channel.Type == discordgo.ChannelTypeGroupDM {
					if !readstate.IsChannelMuted(channel) {
						window.app.QueueUpdateDraw(func() {
							window.privateList.MarkChannelAsUnread(channel)
						})
					}
				} else if channel.Type == discordgo.ChannelTypeGuildText {
					if mentionsCurrentUser {
						window.app.QueueUpdateDraw(func() {
							window.channelTree.MarkChannelAsMentioned(channel.ID)
						})
					} else if !readstate.IsChannelMuted(channel) {
						window.app.QueueUpdateDraw(func() {
							window.channelTree.MarkChannelAsUnread(channel.ID)
						})
					}
				}
			}
		}
	}()

	go func() {
		for messageDeleted := range delete {
			tempMessageDeleted := messageDeleted
			window.session.State.MessageRemove(tempMessageDeleted)
			window.chatView.Lock()
			if window.selectedChannel != nil && window.selectedChannel.ID == tempMessageDeleted.ChannelID {
				window.QueueUpdateDrawSynchronized(func() {
					window.chatView.DeleteMessage(tempMessageDeleted)
				})
			}
			window.chatView.Unlock()
		}
	}()

	go func() {
		for messagesDeleted := range bulkDelete {
			tempMessagesDeleted := messagesDeleted
			for _, messageID := range messagesDeleted.Messages {
				message, stateError := window.session.State.Message(tempMessagesDeleted.ChannelID, messageID)
				if stateError == nil {
					window.session.State.MessageRemove(message)
				}
			}

			window.chatView.Lock()
			if window.selectedChannel != nil && window.selectedChannel.ID == tempMessagesDeleted.ChannelID {
				window.QueueUpdateDrawSynchronized(func() {
					window.chatView.DeleteMessages(tempMessagesDeleted.Messages)
				})
			}
			window.chatView.Unlock()
		}
	}()

	go func() {
		for messageEdited := range edit {
			tempMessageEdited := messageEdited
			window.session.State.MessageAdd(tempMessageEdited)
			window.chatView.Lock()
			if window.selectedChannel != nil && window.selectedChannel.ID == tempMessageEdited.ChannelID {
				for _, message := range window.chatView.data {
					if message.ID == tempMessageEdited.ID {
						//FIXME Workaround for the fact that discordgo doesn't update already filled fields.
						message.Content = tempMessageEdited.Content
						message.Mentions = tempMessageEdited.Mentions
						message.MentionRoles = tempMessageEdited.MentionRoles
						message.MentionEveryone = tempMessageEdited.MentionEveryone

						window.QueueUpdateDrawSynchronized(func() {
							window.chatView.UpdateMessage(message)
						})
						break
					}
				}
			}
			window.chatView.Unlock()
		}
	}()
}

func (window *Window) registerGuildHandlers() {
	//Using buffered channels with a size of three, since this shouldn't really happen often

	guildCreateChannel := make(chan *discordgo.GuildCreate, 3)
	window.session.AddHandler(func(s *discordgo.Session, guildCreate *discordgo.GuildCreate) {
		guildCreateChannel <- guildCreate
	})

	guildRemoveChannel := make(chan *discordgo.GuildDelete, 3)
	window.session.AddHandler(func(s *discordgo.Session, guildRemove *discordgo.GuildDelete) {
		guildRemoveChannel <- guildRemove
	})

	guildUpdateChannel := make(chan *discordgo.GuildUpdate, 3)
	window.session.AddHandler(func(s *discordgo.Session, guildUpdate *discordgo.GuildUpdate) {
		guildUpdateChannel <- guildUpdate
	})

	go func() {
		for guildCreate := range guildCreateChannel {
			guild := guildCreate
			window.app.QueueUpdateDraw(func() {
				window.guildList.AddGuild(guild.ID, guild.Name)
			})
		}
	}()

	go func() {
		for guildUpdate := range guildUpdateChannel {
			guild := guildUpdate
			window.app.QueueUpdateDraw(func() {
				window.guildList.UpdateName(guild.ID, guild.Name)
			})
		}
	}()

	go func() {
		for guildRemove := range guildRemoveChannel {
			if window.selectedGuildNode == nil {
				continue
			}

			if window.previousGuildNode != nil && window.previousGuildNode.GetReference() == guildRemove.ID {
				window.previousGuildNode = nil
				window.previousGuild = nil
				window.previousChannelNode = nil
				window.previousChannel = nil
			}

			if window.selectedGuildNode.GetReference() == guildRemove.ID {
				guildID := guildRemove.ID
				window.app.QueueUpdateDraw(func() {
					if window.selectedChannel != nil && window.selectedChannel.GuildID == guildID {
						window.chatView.ClearViewAndCache()
						window.selectedChannel = nil
						window.selectedChannelNode = nil
					}

					window.channelTree.Clear()
					window.userList.Clear()
					window.guildList.RemoveGuild(guildID)
					window.selectedGuildNode = nil
					window.selectedGuild = nil
				})
			}
		}
	}()
}

func (window *Window) registerGuildMemberHandlers() {
	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.GuildMembersChunk) {
		if window.selectedGuild != nil && window.selectedGuild.ID == event.GuildID {
			window.app.QueueUpdateDraw(func() {
				window.userList.AddOrUpdateMembers(event.Members)
			})
		}
	})

	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.GuildMemberRemove) {
		if window.selectedGuild != nil && window.selectedGuild.ID == event.GuildID {
			window.app.QueueUpdateDraw(func() {
				window.userList.RemoveMember(event.Member)
			})
		}
	})

	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.GuildMemberAdd) {
		if window.selectedGuild != nil && window.selectedGuild.ID == event.GuildID {
			window.app.QueueUpdateDraw(func() {
				window.userList.AddOrUpdateMember(event.Member)
			})
		}
	})

	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.GuildMemberUpdate) {
		if window.selectedGuild != nil && window.selectedGuild.ID == event.GuildID {
			window.app.QueueUpdateDraw(func() {
				window.userList.AddOrUpdateMember(event.Member)
			})
		}
	})
}

func (window *Window) registerPrivateChatsHandler() {
	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.ChannelCreate) {
		if event.Type == discordgo.ChannelTypeDM || event.Type == discordgo.ChannelTypeGroupDM {
			window.app.QueueUpdateDraw(func() {
				window.privateList.AddOrUpdateChannel(event.Channel)
			})
		}
	})

	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.ChannelDelete) {
		if event.Type == discordgo.ChannelTypeDM || event.Type == discordgo.ChannelTypeGroupDM {
			window.app.QueueUpdateDraw(func() {
				window.privateList.RemoveChannel(event.Channel)
			})
			readstate.ClearReadStateFor(event.ID)
		}
	})

	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.ChannelUpdate) {
		if event.Type == discordgo.ChannelTypeDM || event.Type == discordgo.ChannelTypeGroupDM {
			window.app.QueueUpdateDraw(func() {
				window.privateList.AddOrUpdateChannel(event.Channel)
			})
		}
	})

	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.RelationshipAdd) {
		if event.Relationship.Type == discordgo.RelationTypeFriend {
			window.app.QueueUpdateDraw(func() {
				window.privateList.addFriend(event.User)
			})
		}
	})

	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.RelationshipRemove) {
		if event.Relationship.Type == discordgo.RelationTypeFriend {
			for _, relationship := range window.session.State.Relationships {
				if relationship.ID == event.ID {
					window.app.QueueUpdateDraw(func() {
						window.privateList.RemoveFriend(relationship.User.ID)
					})
					break
				}
			}
		}
	})
}

func (window *Window) isChannelEventRelevant(channelEvent *discordgo.Channel) bool {
	if window.selectedGuild == nil {
		return false
	}

	if channelEvent.Type != discordgo.ChannelTypeGuildText && channelEvent.Type != discordgo.ChannelTypeGuildCategory {
		return false
	}

	if window.selectedGuild.ID != channelEvent.GuildID {
		return false
	}

	return true
}

func (window *Window) registerGuildChannelHandler() {
	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.ChannelCreate) {
		if window.isChannelEventRelevant(event.Channel) {
			window.channelTree.Lock()
			window.QueueUpdateDrawSynchronized(func() {
				window.channelTree.AddOrUpdateChannel(event.Channel)
			})
			window.channelTree.Unlock()
		}
	})

	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.ChannelUpdate) {
		if window.isChannelEventRelevant(event.Channel) {
			window.channelTree.Lock()
			window.QueueUpdateDrawSynchronized(func() {
				window.channelTree.AddOrUpdateChannel(event.Channel)
			})
			window.channelTree.Unlock()
		}
	})

	window.session.AddHandler(func(s *discordgo.Session, event *discordgo.ChannelDelete) {
		if window.isChannelEventRelevant(event.Channel) {
			if window.previousChannelNode != nil && window.previousChannelNode.GetReference() == event.ID {
				window.previousGuildNode = nil
				window.previousGuild = nil
				window.previousChannelNode = nil
				window.previousChannel = nil
			}

			if window.selectedChannelNode != nil && window.selectedChannelNode.GetReference() == event.ID {
				window.selectedChannel = nil
				window.selectedChannelNode = nil
				window.app.QueueUpdateDraw(func() {
					window.chatView.ClearViewAndCache()
				})
			}

			//On purpose, since we don't care much about removing the channel timely.
			window.app.QueueUpdateDraw(func() {
				window.channelTree.Lock()
				window.channelTree.RemoveChannel(event.Channel)
				window.channelTree.Unlock()
			})
		}
	})
}

func (window *Window) askForMessageDeletion(messageID string, usedWithSelection bool) {
	deleteButtonText := "Delete"
	window.ShowDialog(tview.Styles.PrimitiveBackgroundColor,
		"Do you really want to delete the message?", func(button string) {
			if button == deleteButtonText {
				go window.session.ChannelMessageDelete(window.selectedChannel.ID, messageID)
			}

			window.exitMessageEditMode()
			if usedWithSelection {
				window.chatView.SignalSelectionDeleted()
			}
		}, deleteButtonText, "Abort")
}

// SetCommandModeEnabled hides or shows the command ui elements and toggles
// the commandMode flag.
func (window *Window) SetCommandModeEnabled(enabled bool) {
	if window.commandMode != enabled {
		window.commandMode = enabled
		window.commandView.SetVisible(enabled)
	}
}

func (window *Window) handleGlobalShortcuts(event *tcell.EventKey) *tcell.EventKey {
	if shortcuts.ExitApplication.Equals(event) {
		window.doRestart <- false
		window.app.Stop()
		return nil
	}

	// Maybe compare directly to table?
	if window.currentContainer != window.rootContainer {
		return event
	}

	window.userActive = true
	window.userActiveTimer.Reset(userInactiveTime)

	if window.dialogReplacement.IsVisible() {
		return event
	}

	if event.Modifiers()&tcell.ModAlt == tcell.ModAlt && event.Rune() == 'S' {
		var table *shortcuts.ShortcutTable
		var shortcutDescription *tview.TextView
		var exitButton *tview.Button
		var resetButton *tview.Button

		table = shortcuts.NewShortcutTable()
		table.SetShortcuts(shortcuts.Shortcuts)

		doClose := func() {
			window.app.SetRoot(window.rootContainer, true)
			window.currentContainer = window.rootContainer
			window.app.ForceDraw()
		}
		table.SetOnClose(doClose)

		exitButton = tview.NewButton("Go back")
		exitButton.SetSelectedFunc(doClose)
		exitButton.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			if event.Key() == tcell.KeyTab {
				window.app.SetFocus(table.GetPrimitive())
			} else if event.Key() == tcell.KeyBacktab {
				window.app.SetFocus(resetButton)
			} else if event.Key() == tcell.KeyESC {
				doClose()
			}

			return event
		})

		resetButton = tview.NewButton("Restore all defaults")
		resetButton.SetSelectedFunc(func() {
			for _, shortcut := range shortcuts.Shortcuts {
				shortcut.Reset()
			}
			shortcuts.Persist()

			table.SetShortcuts(shortcuts.Shortcuts)
			window.app.ForceDraw()
		})
		resetButton.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			if event.Key() == tcell.KeyTab {
				window.app.SetFocus(exitButton)
			} else if event.Key() == tcell.KeyBacktab {
				window.app.SetFocus(table.GetPrimitive())
			} else if event.Key() == tcell.KeyESC {
				doClose()
			}

			return event
		})

		primitiveBGColor := tviewutil.ColorToHex(config.GetTheme().PrimitiveBackgroundColor)
		primaryTextColor := tviewutil.ColorToHex(config.GetTheme().PrimaryTextColor)

		shortcutDescription = tview.NewTextView()
		shortcutDescription.SetDynamicColors(true)
		shortcutDescription.SetText("[" + primaryTextColor + "][:" + primitiveBGColor + "]R [:" + primaryTextColor + "][" + primitiveBGColor + "]Reset shortcut" +
			"[" + primaryTextColor + "][:" + primitiveBGColor + "]  Backspace [:" + primaryTextColor + "][" + primitiveBGColor + "]Delete shortcut" +
			"[" + primaryTextColor + "][:" + primitiveBGColor + "]  Enter [:" + primaryTextColor + "][" + primitiveBGColor + "]Change shortcut" +
			"[" + primaryTextColor + "][:" + primitiveBGColor + "]  Esc [:" + primaryTextColor + "][" + primitiveBGColor + "]Close dialog")

		table.SetFocusNext(func() { window.app.SetFocus(resetButton) })
		table.SetFocusPrevious(func() { window.app.SetFocus(exitButton) })

		buttonBar := tview.NewFlex()
		buttonBar.SetDirection(tview.FlexColumn)

		buttonBar.AddItem(resetButton, 0, 1, false)
		buttonBar.AddItem(tview.NewBox(), 1, 0, false)
		buttonBar.AddItem(exitButton, 0, 1, false)

		shortcutsView := tview.NewFlex()
		shortcutsView.SetDirection(tview.FlexRow)

		shortcutsView.AddItem(table.GetPrimitive(), 0, 1, false)
		shortcutsView.AddItem(buttonBar, 1, 0, false)
		shortcutsView.AddItem(shortcutDescription, 1, 0, false)

		window.app.SetRoot(shortcutsView, true)
		window.app.SetFocus(table.GetPrimitive())
		window.currentContainer = shortcutsView
	} else if shortcuts.ToggleCommandView.Equals(event) {
		window.SetCommandModeEnabled(!window.commandMode)

		if window.commandMode {
			window.app.SetFocus(window.commandView.commandInput.internalTextView)
		} else {
			window.app.SetFocus(window.messageInput.GetPrimitive())
		}
	} else if shortcuts.FocusCommandOutput.Equals(event) {
		if !window.commandMode {
			window.SetCommandModeEnabled(true)
		}

		window.app.SetFocus(window.commandView.commandOutput)
	} else if shortcuts.FocusCommandInput.Equals(event) {
		if !window.commandMode {
			window.SetCommandModeEnabled(true)
		}

		window.app.SetFocus(window.commandView.commandInput.internalTextView)
	} else if shortcuts.ToggleUserContainer.Equals(event) {
		conf := config.GetConfig()
		conf.ShowUserContainer = !conf.ShowUserContainer

		if !conf.ShowUserContainer && window.app.GetFocus() == window.userList.internalTreeView {
			window.app.SetFocus(window.messageInput.GetPrimitive())
		}

		config.PersistConfig()
		window.RefreshLayout()
	} else if shortcuts.FocusChannelContainer.Equals(event) {
		window.SwitchToGuildsPage()
		window.app.SetFocus(window.channelTree)
	} else if shortcuts.FocusPrivateChatPage.Equals(event) {
		window.SwitchToFriendsPage()
		window.app.SetFocus(window.privateList.GetComponent())
	} else if shortcuts.SwitchToPreviousChannel.Equals(event) {
		err := window.SwitchToPreviousChannel()
		if err != nil {
			window.ShowErrorDialog(err.Error())
		}
	} else if shortcuts.FocusGuildContainer.Equals(event) {
		window.SwitchToGuildsPage()
		window.app.SetFocus(window.guildList)
	} else if shortcuts.FocusMessageContainer.Equals(event) {
		window.app.SetFocus(window.chatView.internalTextView)
	} else if shortcuts.FocusUserContainer.Equals(event) {
		if window.leftArea.GetCurrentPage() == guildPageName && window.userList.internalTreeView.IsVisible() {
			window.app.SetFocus(window.userList.internalTreeView)
		}
	} else if shortcuts.FocusMessageInput.Equals(event) {
		window.app.SetFocus(window.messageInput.GetPrimitive())
	} else {
		return event
	}

	return nil
}

func (window *Window) FindCommand(name string) commands.Command {
	for _, cmd := range window.commands {
		if cmd.Name() == name {
			return cmd
		}

		for _, alias := range cmd.Aliases() {
			if alias == name {
				return cmd
			}
		}
	}

	return nil
}

//ExecuteCommand tries to execute the given input as a command. The first word
//will be passed as the commands name and the rest will be parameters. If a
//command can't be found, that info will be printed onto the command output.
func (window *Window) ExecuteCommand(input string) {
	parts := commands.ParseCommand(input)
	fmt.Fprintf(window.commandView, "[gray]$ %s\n", input)

	if len(parts) > 0 {
		command := window.FindCommand(parts[0])
		if command != nil {
			command.Execute(window.commandView, parts[1:])
		} else {
			fmt.Fprintf(window.commandView, "["+tviewutil.ColorToHex(config.GetTheme().ErrorColor)+"]The command '%s' doesn't exist[white]\n", parts[0])
		}
	}
}

func (window *Window) startEditingMessage(message *discordgo.Message) {
	if message.Author.ID == window.session.State.User.ID {
		window.messageInput.SetText(message.Content)
		window.messageInput.SetBorderColor(tcell.ColorYellow)
		window.messageInput.SetBorderFocusColor(tcell.ColorYellow)
		window.editingMessageID = &message.ID
		window.app.SetFocus(window.messageInput.GetPrimitive())
	}
}

func (window *Window) exitMessageEditMode() {
	window.exitMessageEditModeAndKeepText()
	window.messageInput.SetText("")
}

func (window *Window) exitMessageEditModeAndKeepText() {
	window.editingMessageID = nil
	window.messageInput.SetBorderColor(tview.Styles.BorderColor)
	window.messageInput.SetBorderFocusColor(tview.Styles.BorderFocusColor)
}

//ShowErrorDialog shows a simple error dialog that has only an Okay button,
// a generic title and the given text.
func (window *Window) ShowErrorDialog(text string) {
	window.ShowDialog(config.GetTheme().ErrorColor, "An error occured - "+text, func(_ string) {}, "Okay")
}

func (window *Window) editMessage(channelID, messageID, messageEdited string) {
	go func() {
		window.app.QueueUpdateDraw(func() {
			window.exitMessageEditMode()
			window.messageInput.SetText("")
		})
		_, discordError := window.session.ChannelMessageEdit(channelID, messageID, messageEdited)
		window.app.QueueUpdateDraw(func() {
			if discordError != nil {
				retry := "Retry sending"
				edit := "Edit"
				cancel := "Cancel"
				window.ShowDialog(config.GetTheme().ErrorColor,
					fmt.Sprintf("Error editing message: %s.\n\nWhat do you want to do?", discordError),
					func(button string) {
						switch button {
						case retry:
							window.editMessage(channelID, messageID, messageEdited)
						case edit:
							window.messageInput.SetText(messageEdited)
						}
					}, retry, edit, cancel)
			}
		})
	}()
}

//SwitchToGuildsPage the left side of the layout over to the view where you can
//see the servers and their channels. In additional to that, it also shows the
//user list in case the user didn't explicitly hide it.
func (window *Window) SwitchToGuildsPage() {
	if window.leftArea.GetCurrentPage() != guildPageName {
		window.leftArea.SwitchToPage(guildPageName)
		window.RefreshLayout()
	}
}

//SwitchToFriendsPage switches the left side of the layout over to the view
//where you can see your private chats and groups. In addition to that it
//hides the user list.
func (window *Window) SwitchToFriendsPage() {
	if window.leftArea.GetCurrentPage() != privatePageName {
		window.leftArea.SwitchToPage(privatePageName)
		window.RefreshLayout()
	}
}

// Switches to the previous channel and layout.
func (window *Window) SwitchToPreviousChannel() error {
	if window.previousChannel == nil || window.previousChannel == window.selectedChannel {
		// No previous channel.
		return nil
	}

	_, err := window.session.State.Channel(window.previousChannel.ID)
	if err != nil {
		window.previousChannel = nil
		window.previousChannelNode = nil
		return fmt.Errorf("Channel %s not found", window.previousChannel.Name)
	}

	// Switch to appropriate layout.
	switch window.previousChannel.Type {
	case discordgo.ChannelTypeDM, discordgo.ChannelTypeGroupDM:
		if window.leftArea.GetCurrentPage() != privatePageName {
			window.leftArea.SwitchToPage(privatePageName)
		}
		window.privateList.onChannelSelect(window.previousChannelNode, window.previousChannel.ID)
	case discordgo.ChannelTypeGuildText:
		_, err := window.session.State.Guild(window.previousGuild.ID)
		if err != nil {
			window.previousGuild = nil
			window.previousGuildNode = nil
			return fmt.Errorf("Unable to load guild: %s", window.previousGuild.Name)
		}
		if !discordutil.HasReadMessagesPermission(window.previousChannel.ID, window.session.State) {
			return fmt.Errorf("No read permissions for channel: %s", window.previousChannel.Name)
		}
		// Select guild.
		if window.leftArea.GetCurrentPage() != guildPageName {
			window.leftArea.SwitchToPage(guildPageName)
		}
		window.guildList.SetCurrentNode(window.previousGuildNode)
		window.guildList.onGuildSelect(window.previousGuildNode, window.previousGuild.ID)
		window.channelTree.SetCurrentNode(window.previousChannelNode)
		window.channelTree.onChannelSelect(window.previousChannel.ID)
	default:
		return fmt.Errorf("Invalid channel type: %v", window.previousChannel.Type)
	}
	window.app.SetFocus(window.messageInput.internalTextView)
	return nil
}

//RefreshLayout removes and adds the main parts of the layout
//so that the ones that are disabled by settings do not show up.
func (window *Window) RefreshLayout() {
	conf := config.GetConfig()

	window.userList.internalTreeView.SetVisible(conf.ShowUserContainer && (window.selectedGuild != nil ||
		(window.selectedChannel != nil && window.selectedChannel.Type == discordgo.ChannelTypeGroupDM)))

	if conf.UseFixedLayout {
		window.middleContainer.ResizeItem(window.leftArea, conf.FixedSizeLeft, 7)
		window.middleContainer.ResizeItem(window.chatArea, 0, 1)
		window.middleContainer.ResizeItem(window.userList.internalTreeView, conf.FixedSizeRight, 6)
	} else {
		window.middleContainer.ResizeItem(window.leftArea, 0, 7)
		window.middleContainer.ResizeItem(window.chatArea, 0, 20)
		window.middleContainer.ResizeItem(window.userList.internalTreeView, 0, 6)
	}

	window.app.ForceDraw()
}

//LoadChannel eagerly loads the channels messages.
func (window *Window) LoadChannel(channel *discordgo.Channel) error {
	var messages []*discordgo.Message

	if channel.LastMessageID != "" && len(channel.Messages) == 0 {
		cache, cacheError := window.session.State.Channel(channel.ID)
		if cacheError == nil || cache != nil && len(cache.Messages) == 0 {
			var discordError error
			messages, discordError = window.session.ChannelMessages(channel.ID, 100, "", "", "")
			if discordError == nil {
				if channel.GuildID != "" {
					for _, message := range messages {
						message.GuildID = channel.GuildID
					}
				}
				cache.Messages = append(cache.Messages, messages...)
			}
		} else {
			messages = make([]*discordgo.Message, 0)
		}
	} else {
		messages = channel.Messages
	}

	discordutil.SortMessagesByTimestamp(messages)

	window.chatView.SetMessages(messages)
	window.chatView.ClearSelection()
	window.chatView.internalTextView.ScrollToEnd()

	window.UpdateChatHeader(channel)

	if window.selectedChannel == nil {
		window.previousChannel = channel
		window.previousChannelNode = window.channelTree.GetCurrentNode()
	} else if channel != window.selectedChannel {
		window.previousChannel = window.selectedChannel
		window.previousChannelNode = window.selectedChannelNode

		// When switching to a channel in the same guild, the previousGuild must be set.
		if window.previousChannel.GuildID == channel.GuildID {
			window.previousGuild = window.selectedGuild
			window.previousGuildNode = window.selectedGuildNode
		}
	}

	//If there is a  currently loaded guild channel and it isn't the same as
	//the new one we assume it must be read and mark it white.
	if window.selectedChannelNode != nil && channel.ID != window.selectedChannel.ID {
		window.selectedChannelNode.SetColor(tview.Styles.PrimaryTextColor)
	}

	window.selectedChannel = channel
	//FIXME this is a bit bad, since it could be wrong
	window.selectedChannelNode = window.channelTree.GetCurrentNode()

	//Unlike with the channel, where we can assume it is read, we gotta check
	//whether there is still an unread channel and mark the server accordingly.
	if window.selectedGuild != nil && window.selectedGuild.ID != channel.GuildID {
		window.updateServerReadStatus(window.selectedGuild.ID, window.selectedGuildNode, false)
	}

	if channel.GuildID == "" {
		window.selectedGuild = nil
		window.selectedGuildNode = nil
	}

	if channel.Type == discordgo.ChannelTypeDM || channel.Type == discordgo.ChannelTypeGroupDM {
		window.privateList.MarkChannelAsLoaded(channel)
	}

	window.exitMessageEditModeAndKeepText()

	if config.GetConfig().FocusMessageInputAfterChannelSelection {
		window.app.SetFocus(window.messageInput.internalTextView)
	}

	go func() {
		readstate.UpdateRead(window.session, channel, channel.LastMessageID)

		// Here we make the assumption that the channel we are loading must be part
		// of the currently loaded guild, since we don't allow loading a channel of
		// a guilder otherwise.
		if channel.GuildID != "" {
			guild, cacheError := window.session.State.Guild(channel.GuildID)
			if cacheError == nil {
				window.selectedGuild = guild
				window.app.QueueUpdateDraw(func() {
					for _, guildNode := range window.guildList.GetRoot().GetChildren() {
						if guildNode.GetReference() == channel.GuildID {
							window.guildList.SetCurrentNode(guildNode)
							guildNode.SetColor(tview.Styles.ContrastBackgroundColor)
							window.selectedGuildNode = guildNode
							break
						}
					}
				})
			}
		}
	}()

	return nil
}

// UpdateChatHeader updates the bordertitle of the chatviews container.o
// The title consist of the channel name and its topic for guild channels.
// For private channels it's either the recipient in a dm, or all recipients
// in a group dm channel. If the channel has a nickname, that is chosen.
func (window *Window) UpdateChatHeader(channel *discordgo.Channel) {
	if channel == nil {
		return
	}

	if channel.Type == discordgo.ChannelTypeGuildText {
		if channel.Topic != "" {
			window.chatView.SetTitle(channel.Name + " - " + channel.Topic)
		} else {
			window.chatView.SetTitle(channel.Name)
		}
	} else if channel.Type == discordgo.ChannelTypeDM {
		window.chatView.SetTitle(channel.Recipients[0].Username)
	} else {
		window.chatView.SetTitle(discordutil.GetPrivateChannelName(channel))
	}
}

// RegisterCommand register a command. That makes the command available for
// being called from the message input field, in case the user-defined prefix
// is in front of the input.
func (window *Window) RegisterCommand(command commands.Command) {
	window.commands = append(window.commands, command)
}

// GetRegisteredCommands returns the map of all registered commands.
func (window *Window) GetRegisteredCommands() []commands.Command {
	//FIXME eh, should this be a copy?
	return window.commands
}

// GetSelectedGuild returns a reference to the currently selected Guild.
func (window *Window) GetSelectedGuild() *discordgo.Guild {
	return window.selectedGuild
}

// GetSelectedChannel returns a reference to the currently selected Channel.
func (window *Window) GetSelectedChannel() *discordgo.Channel {
	return window.selectedChannel
}

// PromptSecretInput shows an input dialog that masks the user input. The
// returned value will either be empty or what the user has entered.
func (window *Window) PromptSecretInput(title, message string) string {
	waitChannel := make(chan struct{})
	var output string
	var previousFocus tview.Primitive
	window.app.QueueUpdateDraw(func() {
		previousFocus = window.app.GetFocus()
		inputField := tview.NewInputField()
		inputField.SetMaskCharacter('*')
		inputField.SetDoneFunc(func(key tcell.Key) {
			if key == tcell.KeyEnter {
				output = inputField.GetText()
				waitChannel <- struct{}{}
			} else if key == tcell.KeyEscape {
				waitChannel <- struct{}{}
			}
		})
		inputField.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			//FIXME Use shortcut and make it proper PasteAtCursor/Selection
			if event.Key() == tcell.KeyCtrlV {
				content, clipError := clipboard.ReadAll()
				if clipError == nil {
					inputField.SetText(content)
				}
				return nil
			}

			return event
		})
		frame := tview.NewFrame(inputField)
		frame.SetTitle(title)
		frame.SetBorder(true)
		frame.AddText(message, true, tview.AlignLeft, tcell.ColorDefault)
		window.app.SetRoot(frame, true)
		window.currentContainer = frame
	})
	<-waitChannel
	window.app.QueueUpdateDraw(func() {
		window.app.SetRoot(window.rootContainer, true)
		window.currentContainer = window.rootContainer
		window.app.SetFocus(previousFocus)
		waitChannel <- struct{}{}
	})
	<-waitChannel
	return output
}

// ForceRedraw triggers ForceDraw on the underlying tview application, causing
// it to redraw all currently shown components.
func (window *Window) ForceRedraw() {
	window.app.ForceDraw()
}

//Run Shows the window optionally returning an error.
func (window *Window) Run() error {
	return window.app.Run()
}

// Shutdown disconnects from the discord API and stops the tview application.
func (window *Window) Shutdown() {
	if config.GetConfig().ShortenLinks {
		window.chatView.shortener.Close()
	}
	window.session.Close()
	window.app.Stop()
}
