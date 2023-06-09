package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"soa_project/client/game"
	"soa_project/pkg/proto/mafia"
	"soa_project/utils/algo"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gdamore/tcell"
	"github.com/streadway/amqp"

	mapset "github.com/deckarep/golang-set/v2"
)

const (
	exchangeName = "message_exchange"
)

const (
	cursorSymbol = '\u2588'
	cmdPrefix    = "/>"

	mafiaGameName     = "MAFIA GAME"
	autoMafiaGameName = "MAFIA GAME [AUTOMATIC MODE]"

	enterNamePrefix     = "[ENTER YOUR NAME:]"
	waitForOthersPrefix = "[WAIT FOR OTHER PLAYERS]"

	shouldKillPrefix   = "CHOOSE PLAYER TO KILL"
	shouldSearchPrefix = "CHOOSE PLAYER TO CHECK"
	shouldVotePrefix   = "CHOOSE PLAYER TO VOTE"

	successWaitPrefix      = "[SUCCESS, WAIT OTHERS]"
	voteOrFinishChatPrefix = "[/vote or /finish. CHAT]"
	finishChatPrefix       = "[/finish the day. CHAT]"
	mafiaChatPrefix        = "[/kill required. MAFIA CHAT]"

	deadChillingPrefix = "[YOU ARE DEAD, WAIT OR EXIT]"

	finishRequiredPrefix = "[/finish the day]"

	gameFinishedPrefix = "[GAME FINISHED]"
)

var (
	validateError = errors.New("validate")
)

type Client struct {
	state clientState

	grpcClient       mafia.MafiaClient
	gameEventsChan   chan *mafia.Event
	gameEventsStream mafia.Mafia_RegisterClient

	chatConn        *amqp.Connection
	chatChan        chan game.Message
	chatPublishChan *amqp.Channel
	dropChatChan    chan bool

	screen           tcell.Screen
	screenEventsChan chan tcell.Event

	connectionFailedChan chan int
}

type clientState struct {
	roomId int

	myName string

	input       string
	inputPrefix string
	inputError  string

	gameState     game.GameState
	updatesBuffer *CircularBuffer
	waitingRoom   []string
	players       map[string]mafia.MafiaRole
	states        map[string]mafia.MafiaState
	gameTime      game.TimeType
	dayNum        int

	commandsBuffer    *CircularBuffer
	availableCommands mapset.Set[string]

	autoMode bool
	lastAsk  game.AskQueue
}

func NewClient(screen tcell.Screen, cli mafia.MafiaClient, chatConn *amqp.Connection) *Client {
	return &Client{
		state: clientState{
			input:             "",
			gameState:         game.StateRegister,
			updatesBuffer:     NewCircularBuffer(10),
			commandsBuffer:    NewCircularBuffer(100),
			waitingRoom:       []string{},
			players:           make(map[string]mafia.MafiaRole),
			states:            make(map[string]mafia.MafiaState),
			availableCommands: mapset.NewSet(game.ExitCommandDesc),
			dayNum:            1,
			lastAsk:           game.NewAskQueue(),
		},
		screen:               screen,
		connectionFailedChan: make(chan int),
		grpcClient:           cli,
		gameEventsChan:       make(chan *mafia.Event),
		screenEventsChan:     make(chan tcell.Event),
		chatConn:             chatConn,
		chatChan:             make(chan game.Message),
		dropChatChan:         make(chan bool),
	}
}

func (c *Client) Start() {
	go c.listenScreenEvents()
	c.handleInput()
}

func (c *Client) listenScreenEvents() {
	for {
		ev := c.screen.PollEvent()
		c.screenEventsChan <- ev
	}
}

func (c *Client) listenGameEvents() {
	for {
		ev, err := c.gameEventsStream.Recv()
		if err != nil {
			log.Println("ERROR: ", err.Error())
			c.connectionFailedChan <- 0
			return
		}
		c.gameEventsChan <- ev
	}
}

func (c *Client) handleInput() {
	c.reRenderScreen()
	for {
		select {
		case ev := <-c.gameEventsChan:
			c.handleGameEvent(ev)
		case ev := <-c.screenEventsChan:
			c.handleScreenEvent(ev)
		case msg := <-c.chatChan:
			c.handleChatMessage(msg)
		case <-c.connectionFailedChan:
			c.handleConnectionFailed()
		}

		c.reRenderScreen()
	}
}

func (c *Client) handleConnectionFailed() {
	c.state.inputError = "FAILED TO CONNECT. ENTER ANOTHER NAME"
	c.state.gameState = game.StateRegister

	c.reRenderScreen()
}

func (c *Client) handleGameEvent(ev *mafia.Event) {
	switch event := ev.Data.(type) {
	case *mafia.Event_Connect:
		c.state.updatesBuffer.Add(fmt.Sprintf("[CONNECTED]: %s", event.Connect.GetPersonName()))
		c.state.waitingRoom = append(c.state.waitingRoom, event.Connect.GetPersonName())
	case *mafia.Event_Disconnect:
		c.state.updatesBuffer.Add(fmt.Sprintf("[DISCONNECTED]: %s", event.Disconnect.GetPersonName()))
		c.removeFromWaitingRoom(event.Disconnect.GetPersonName())
	case *mafia.Event_HelloMessage:
		c.state.updatesBuffer.Add(fmt.Sprintf("[SYSTEM]: %s", event.HelloMessage))
		c.state.waitingRoom = append(c.state.waitingRoom, c.state.myName)
	case *mafia.Event_Ping:
		log.Println("ping received")
	case *mafia.Event_GameStarted:
		c.onGameStarted(event.GameStarted)
	case *mafia.Event_AskKill:
		c.onAskKill(event.AskKill)
	case *mafia.Event_AskSearch:
		c.onAskSearch(event.AskSearch)
	case *mafia.Event_AskVote:
		c.onAskVote(event.AskVote)
	case *mafia.Event_DayStarted:
		c.onDayStarted(event.DayStarted)
	case *mafia.Event_NightStarted:
		c.onNightStarted(event.NightStarted)
	case *mafia.Event_VotingCompleted:
		c.onVotingCompleted(event.VotingCompleted)
	case *mafia.Event_GameFinished:
		c.onGameFinished(event.GameFinished)
	default:
		log.Fatalln("unexpected")
	}

	c.reRenderScreen()
}

func (c *Client) handleChatMessage(msg game.Message) {
	if c.state.gameTime == game.TimeNight {
		// at night only mafias can chat
		if c.state.players[c.state.myName] != mafia.MafiaRole_MAFIA {
			return
		}
		c.state.updatesBuffer.Add(fmt.Sprintf("[MAFIA CHAT, %s]: %s", msg.From, msg.Data))
	} else {
		c.state.updatesBuffer.Add(fmt.Sprintf("[CHAT, %s]: %s", msg.From, msg.Data))
	}
}

func (c *Client) onVotingCompleted(vc *mafia.VotingCompleted) {
	if vc.KilledVictim == nil {
		c.state.updatesBuffer.Add("[SYSTEM]: NOBODY KILLED ON VOTING")
	} else {
		c.state.updatesBuffer.Add(fmt.Sprintf("[SYSTEM]: %s WAS KILLED ON VOTING", *vc.KilledVictim))
		c.state.states[vc.GetKilledVictim()] = mafia.MafiaState_DEAD
	}
}

func (c *Client) onNightStarted(ns *mafia.NightStarted) {
	c.state.updatesBuffer.Add(fmt.Sprintf("[SYSTEM]: NIGHT %d started", ns.GetNightNum()))
	c.state.gameTime = game.TimeNight
	c.state.dayNum = int(ns.GetNightNum())
}

func (c *Client) onGameFinished(g *mafia.GameFinished) {
	c.state.updatesBuffer.Add(fmt.Sprintf("[SYSTEM]: Game finished!"))
	switch g.WinRole {
	case mafia.MafiaRole_CIVILIAN:
		c.state.updatesBuffer.Add("[SYSTEM]: CIVILIANS & SHERIFF WON!")
	case mafia.MafiaRole_MAFIA:
		c.state.updatesBuffer.Add("[SYSTEM]: MAFIA WON!")
	}

	for name, role := range g.GetRoles() {
		c.state.players[name] = role
	}

	c.state.gameState = game.StateChilling
	c.state.inputPrefix = gameFinishedPrefix

	c.state.availableCommands.Remove(game.AutoCommandDesc)
	c.state.availableCommands.Add(game.ReplayCommandDesc)

	c.state.autoMode = false
	c.state.lastAsk.Reset()

	c.dropChatChan <- true
}

func (c *Client) onDayStarted(day *mafia.DayStarted) {
	c.state.dayNum = int(day.GetDayNum())
	c.state.gameTime = game.TimeDay

	c.state.updatesBuffer.Add(fmt.Sprintf("[SYSTEM]: DAY %d started", c.state.dayNum))

	if day.GetKilledVictim() == c.state.myName {
		c.state.updatesBuffer.Add(fmt.Sprintf("[SYSTEM]: YOU ARE KILLED :("))
		c.state.gameState = game.StateChilling
		c.state.inputPrefix = deadChillingPrefix
	} else {
		c.state.updatesBuffer.Add(fmt.Sprintf("[SYSTEM]: player %s was KILLED :(", day.GetKilledVictim()))
	}

	c.state.states[day.GetKilledVictim()] = mafia.MafiaState_DEAD
}

func (c *Client) onAskKill(ask *mafia.Ask) {
	c.state.lastAsk.Set(&mafia.Ask{Default: game.KillCommand})
	c.state.lastAsk.Push(ask)
	c.state.gameState = game.StatePlaying
	c.state.inputPrefix = mafiaChatPrefix

	c.state.availableCommands.Add(game.KillCommandDesc)

	if c.state.autoMode {
		c.state.input = c.state.lastAsk.Pop().GetDefault()
		c.handleEnter()
	}
}

func (c *Client) onKillCommand() {
	c.state.gameState = game.StateKilling
	c.state.inputPrefix = fmt.Sprintf("[%s %d-%d]", shouldKillPrefix, 1, len(c.state.players))
	c.state.availableCommands.Remove(game.KillCommandDesc)

	if c.state.autoMode {
		c.state.input = c.state.lastAsk.Pop().GetDefault()
		c.handleEnter()
	}
}

func (c *Client) onAskVote(ask *mafia.Ask) {
	c.state.lastAsk.Set(&mafia.Ask{Default: game.VoteCommand})
	c.state.lastAsk.Push(ask)
	c.state.gameState = game.StatePlaying
	c.state.inputPrefix = voteOrFinishChatPrefix

	c.state.availableCommands.Add(game.VoteCommandDesc)
	c.state.availableCommands.Add(game.FinishCommandDesc)

	if c.state.autoMode {
		c.state.input = c.state.lastAsk.Pop().GetDefault()
		c.handleEnter()
	}
}

func (c *Client) onVoteCommand() {
	c.state.gameState = game.StateVoting
	c.state.inputPrefix = fmt.Sprintf("[%s %d-%d]", shouldVotePrefix, 1, len(c.state.players))
	c.state.availableCommands.Remove(game.VoteCommandDesc)
	c.state.availableCommands.Remove(game.FinishCommandDesc)

	if c.state.autoMode {
		c.state.input = c.state.lastAsk.Pop().GetDefault()
		c.handleEnter()
	}
}

func (c *Client) onAskSearch(ask *mafia.Ask) {
	c.state.lastAsk.Set(ask)
	c.state.gameState = game.StateSearching
	c.state.inputPrefix = fmt.Sprintf("[%s %d-%d]", shouldSearchPrefix, 1, len(c.state.players))

	if c.state.autoMode {
		c.state.input = ask.GetDefault()
		c.handleEnter()
	}
}

func (c *Client) onGameStarted(gs *mafia.GameStarted) {
	c.state.roomId = int(gs.GetRoomId())
	c.state.updatesBuffer.Clear()
	c.state.updatesBuffer.Add("[SYSTEM]: GAME STARTED")
	c.state.commandsBuffer.Clear()

	for _, name := range gs.GetPlayers() {
		c.state.players[name] = mafia.MafiaRole_UNKNOWN
		c.state.states[name] = mafia.MafiaState_ALIVE
	}

	c.state.players[c.state.myName] = gs.GetRole()

	c.state.gameState = game.StatePlaying
	c.state.gameTime = game.TimeNight
	c.state.dayNum = 1

	wg := &sync.WaitGroup{}
	wg.Add(1)

	go c.joinChatRoom(wg)

	wg.Wait()
}

func (c *Client) joinChatRoom(wg *sync.WaitGroup) {
	ch, err := c.chatConn.Channel()
	defer ch.Close()
	c.chatPublishChan = ch
	defer func() { c.chatPublishChan = nil }()

	err = ch.ExchangeDeclare(
		exchangeName, // exchange name
		"direct",     // exchange type
		true,         // durable
		false,        // auto-deleted
		false,        // internal
		false,        // no-wait
		nil,          // arguments
	)
	if err != nil {
		c.state.inputError = fmt.Errorf("exchange: %w", err).Error()
		c.reRenderScreen()
		return
	}

	q, err := ch.QueueDeclare(
		c.getChatQueueName(c.state.myName), // queue name
		false,                              // durable
		false,                              // delete when unused
		false,                              // exclusive
		false,                              // no-wait
		nil,                                // arguments
	)
	if err != nil {
		c.state.inputError = fmt.Errorf("queuedeclare: %w", err).Error()
		c.reRenderScreen()
		return
	}

	err = ch.QueueBind(
		q.Name,                // queue name
		c.getChatRoutingKey(), // routing key
		exchangeName,          // exchange name
		false,                 // no-wait
		nil,                   // arguments
	)
	if err != nil {
		c.state.inputError = fmt.Errorf("bind: %w", err).Error()
		c.reRenderScreen()
		return
	}

	msgs, err := ch.Consume(
		c.getChatQueueName(c.state.myName), // queue
		"",                                 // consumer
		true,                               // auto-ack
		false,                              // exclusive
		false,                              // no-local
		false,                              // no-wait
		nil,                                // args
	)
	if err != nil {
		c.state.inputError = fmt.Errorf("consume: %w", err).Error()
		c.reRenderScreen()
		return
	}

	wg.Done()

	for {
		select {
		case d := <-msgs:
			m := game.Message{}
			err = json.Unmarshal(d.Body, &m)
			if err != nil {
				c.state.inputError = fmt.Errorf("unmarshall: %w", err).Error()
				c.reRenderScreen()
				return
			}

			c.chatChan <- m

		case <-c.dropChatChan:
			return
		}
	}
}

func (c *Client) removeFromWaitingRoom(name string) {
	for i, n := range c.state.waitingRoom {
		if n == name {
			c.state.waitingRoom = append(c.state.waitingRoom[:i], c.state.waitingRoom[i+1:]...)
			return
		}
	}
}

func (c *Client) handleScreenEvent(ev tcell.Event) {
	switch ev := ev.(type) {
	case *tcell.EventResize:
		c.reRenderScreen()
	case *tcell.EventKey:
		c.handleEventKey(ev)
	}
}

func (c *Client) handleEventKey(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEsc:
		c.leaveGame()
	case tcell.KeyEnter:
		if !c.state.autoMode {
			c.handleEnter()
		}

		c.state.input = ""

	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(c.state.input) > 0 {
			runes := []rune(c.state.input)
			c.state.input = string(runes[:len(runes)-1])
		}
	default:
		c.state.input += string(ev.Rune())
	}

	c.reRenderScreen()
}

func (c *Client) handleEnter() {
	mayBeCommand := strings.TrimSpace(strings.ToLower(c.state.input))
	if mayBeCommand != "" {
		c.state.inputError = ""
	}

	if !c.state.autoMode && c.state.availableCommands.Contains(game.AutoCommandDesc) && mayBeCommand != game.AutoCommand {
		c.state.lastAsk.Pop()
	}

	c.state.input = strings.TrimSpace(c.state.input)
	c.state.commandsBuffer.Add(c.getFullInput())

	found := false
	for cmd := range c.state.availableCommands.Iter() {
		if strings.HasPrefix(cmd, mayBeCommand) {
			found = true
		}
	}
	if found {
		switch mayBeCommand {
		case game.ExitCommand:
			c.leaveGame()
		case game.AutoCommand:
			c.onAutoCommand()
		case game.ReplayCommand:
			c.onReplayCommand()
		case game.FinishCommand:
			c.onFinishCommand()
		case game.KillCommand:
			c.onKillCommand()
		case game.VoteCommand:
			c.onVoteCommand()
		}
	} else {
		switch c.state.gameState {
		case game.StateRegister:
			c.onNameEntered()
		case game.StateKilling:
			c.onKillEntered()
		case game.StateSearching:
			c.onSearchEntered()
		case game.StateVoting:
			c.onVoteEntered()
		case game.StatePlaying:
			c.onChatMessageEntered()
		}
	}

	c.state.input = ""
}

func (c *Client) onChatMessageEntered() {
	// only mafia can chat at night
	if c.state.gameTime == game.TimeNight && c.state.players[c.state.myName] != mafia.MafiaRole_MAFIA {
		return
	}

	msg := c.state.input

	res, err := json.Marshal(game.Message{From: c.state.myName, Data: msg})
	if err != nil {
		c.state.inputError = err.Error()
		return
	}

	err = c.chatPublishChan.Publish(
		exchangeName,          // exchange
		c.getChatRoutingKey(), // routing key
		false,                 // mandatory
		false,                 // immediate
		amqp.Publishing{
			ContentType: "text/plain",
			Body:        res,
		},
	)

	if err != nil {
		c.state.inputError = err.Error()
		return
	}
}

func (c *Client) onAutoCommand() {
	c.state.autoMode = true
	c.state.input = c.state.lastAsk.Pop().GetDefault()

	c.state.availableCommands.Remove(game.AutoCommandDesc)
	c.handleEnter()
}

func (c *Client) onReplayCommand() {
	resp, err := c.grpcClient.JoinWaitingRoom(context.Background(), &mafia.JoinRequest{
		Name: c.state.myName,
	})
	if err != nil {
		c.state.inputError = err.Error()
		return
	}

	c.state.gameState = game.StateWaiting
	c.state.updatesBuffer.Clear()
	c.state.waitingRoom = resp.GetPlayers()

	c.state.availableCommands.Remove(game.ReplayCommandDesc)
	c.state.availableCommands.Add(game.AutoCommandDesc)
}

func (c *Client) onFinishCommand() {
	resp, err := c.grpcClient.FinishDay(context.Background(), &mafia.FinishDayRequest{Name: c.state.myName})
	if err = c.validateActionResp(resp, err); err != nil {
		return
	}

	c.state.availableCommands.Remove(game.FinishCommandDesc)
	c.state.availableCommands.Remove(game.VoteCommandDesc)
	c.state.inputPrefix = successWaitPrefix
}

func (c *Client) onKillEntered() {
	name, err := c.validatePersonName(c.state.input)
	if err != nil {
		c.state.inputError = err.Error()
		return
	}

	resp, err := c.grpcClient.Kill(context.Background(), &mafia.GameRequest{Name: c.state.myName, Victim: name})

	if err = c.validateActionResp(resp, err); err != nil {
		return
	}
}

func (c *Client) onSearchEntered() {
	name, err := c.validatePersonName(c.state.input)
	if err != nil {
		c.state.inputError = err.Error()
		return
	}

	resp, err := c.grpcClient.Search(context.Background(), &mafia.GameRequest{Name: c.state.myName, Victim: name})

	if err = c.validateActionResp(resp, err); err != nil {
		return
	}

	victimRole := resp.GetRole()

	if victimRole == mafia.MafiaRole_UNKNOWN {
		c.state.inputError = "SERVER ERROR UNKNOWN RESULT"
		c.state.gameState = game.StateSearching
		return
	}

	c.state.players[name] = victimRole
	c.state.updatesBuffer.Add(fmt.Sprintf("[SYSTEM]: player %s is %s", name, game.GetRoleName(victimRole)))
}

func (c *Client) onVoteEntered() {
	name, err := c.validatePersonName(c.state.input)
	if err != nil {
		c.state.inputError = err.Error()
		return
	}

	resp, err := c.grpcClient.Vote(context.Background(), &mafia.GameRequest{Name: c.state.myName, Victim: name})

	if err = c.validateActionResp(resp, err); err != nil {
		return
	}
}

func (c *Client) validateActionResp(resp *mafia.GameResponse, err error) error {
	if err != nil {
		c.state.inputError = err.Error()
		return validateError
	}

	if !resp.GetSuccess() {
		c.state.inputError = fmt.Sprintf("ERROR: %s", resp.GetReason())
		return validateError
	}

	c.state.inputPrefix = successWaitPrefix
	c.state.gameState = game.StatePlaying

	return nil
}

func (c *Client) validatePersonName(input string) (string, error) {
	names := []string{}
	for name := range c.state.players {
		names = append(names, name)
		if input == name {
			return name, nil
		}
	}

	sort.Strings(names)

	idx, err := strconv.Atoi(input)
	if err != nil {
		return "", fmt.Errorf("expected name of player of number 1-%d: %w", len(names), err)
	}

	if idx < 1 || idx > len(names) {
		return "", fmt.Errorf("expected number from 1 to %d", len(names))
	}

	return names[idx-1], nil
}

func (c *Client) onNameEntered() {
	if c.state.input == "" {
		c.state.inputError = "NAME MUST NOT BE EMPTY"
		return
	}
	name := c.state.input
	c.state.myName = name

	request := &mafia.RegisterRequest{
		Name: name,
	}

	result, err := c.grpcClient.Register(context.Background(), request)
	if err != nil {
		c.state.inputError = err.Error()

		return
	}

	c.gameEventsStream = result

	c.state.inputError = ""
	c.state.gameState = game.StateWaiting
	c.state.availableCommands.Add(game.AutoCommandDesc)

	go c.listenGameEvents()
}

func (c *Client) leaveGame() {
	c.screen.Fini()
	os.Exit(0)
}

func (c *Client) getChatQueueName(username string) string {
	return fmt.Sprintf("chat_%d_%s", c.state.roomId, username)
}

func (c *Client) getChatRoutingKey() string {
	return fmt.Sprintf("chat_%d", c.state.roomId)
}

func (c *Client) reRenderScreen() {
	screen := c.screen

	screen.Clear()

	screenWidth, screenHeight := screen.Size()

	borderStyle := tcell.StyleDefault
	for row := 1; row < screenHeight-1; row++ {
		screen.SetContent(0, row, '│', nil, borderStyle)
		screen.SetContent(screenWidth/2, row, '│', nil, borderStyle)
		screen.SetContent(screenWidth-1, row, '│', nil, borderStyle)
	}

	// Draw borders
	for col := 0; col < screenWidth; col++ {
		screen.SetContent(col, 0, '─', nil, borderStyle)
		screen.SetContent(col, 2, '─', nil, borderStyle)
		screen.SetContent(col, screenHeight-1, '─', nil, borderStyle)
	}

	if c.state.autoMode {
		c.emitInTheMiddle(1, 1, screenWidth-2, autoMafiaGameName)
	} else {
		c.emitInTheMiddle(1, 1, screenWidth-2, mafiaGameName)
	}

	// Draw split-screen line
	for row := 4; row < screenHeight-1; row++ {
		screen.SetContent(screenWidth/2, row, '│', nil, borderStyle)
	}

	c.reRenderLeftScreen(1, 3, screenWidth/2-1, screenHeight-4)
	c.reRenderRightScreen(screenWidth/2+1, 3, screenWidth-(screenWidth/2-1)-3, screenHeight-4)

	screen.Show()
}

func (c *Client) reRenderLeftScreen(x, y, width, height int) {
	bottomLimit := y + height

	switch c.state.gameState {
	case game.StateRegister:
		c.state.inputPrefix = enterNamePrefix
	case game.StateWaiting:
		c.state.inputPrefix = waitForOthersPrefix
	}

	b := strings.Builder{}
	b.WriteString(c.getFullInput())
	b.WriteRune(cursorSymbol)

	if c.state.inputError != "" {
		eb := strings.Builder{}
		eb.WriteString("ERROR: ")
		eb.WriteString(c.state.inputError)

		c.emitStr(x, y, width, eb.String())
		y++
	}

	commands := c.state.availableCommands.ToSlice()
	sort.Strings(commands)

	bottomRow := bottomLimit - 1
	for _, cmd := range commands {
		c.emitStr(x, bottomRow, width, cmd)
		bottomRow--
	}

	c.emitInTheMiddle(x, bottomRow, width, "COMMANDS:")
	bottomRow--
	c.emitStr(x, bottomRow, width, strings.Repeat("-", width))

	rows := []string{b.String()}
	rows = append(rows, c.state.commandsBuffer.GetReversed()...)

	c.renderRows(x, y, width, bottomRow-y, rows)
}

func (c *Client) renderRows(x, y, width, height int, rows []string) {
	limit := algo.Min(len(rows), height)

	for i := 0; i < limit; i++ {
		c.emitStr(x, y+limit-i-1, width, rows[i])
	}
}

func (c *Client) getFullInput() string {
	return fmt.Sprintf("%s%s%s", c.state.inputPrefix, cmdPrefix, c.state.input)
}

func (c *Client) reRenderRightScreen(x, y, width, height int) {
	// choose right side

	bottomLimit := y + height

	switch c.state.gameState {
	case game.StateWaiting:
		y = c.renderWaitingRoom(x, y, width, height)
	case game.StatePlaying, game.StateKilling, game.StateSearching, game.StateVoting, game.StateChilling:
		y = c.renderGameRoom(x, y, width, height)
	}

	commands := c.state.updatesBuffer.GetReversed()
	c.renderRows(x, y, width, bottomLimit-y, commands)
}

func (c *Client) renderGameRoom(x, y, width, height int) int {
	c.emitStr(x, y, width, strings.Repeat("-", width))
	y++
	c.emitInTheMiddle(x, y, width, fmt.Sprintf("[%s - %d] GAME ROOM:", game.GetTimeName(c.state.gameTime), c.state.dayNum))
	y++

	show := map[string]string{}

	longest := 0
	for name, role := range c.state.players {
		b := strings.Builder{}
		if name == c.state.myName {
			b.WriteString("YOU, ")
		}

		b.WriteString(fmt.Sprintf("%s, %s", game.GetRoleName(role), game.GetAliveStateName(c.state.states[name])))
		show[name] = b.String()

		longest = algo.Max(longest, len(b.String()))
	}

	names := []string{}
	for name := range c.state.players {
		names = append(names, name)
	}

	sort.Strings(names)

	for idx, name := range names {
		beginning := fmt.Sprintf(" %d) ", idx+1)
		c.emitStr(x, y, width, fmt.Sprintf("%s[%s]: %s", beginning, strings.Repeat(" ", longest), name))
		c.emitInTheMiddle(x+len(beginning)+1, y, longest, show[name])
		y++
	}

	c.emitStr(x, y, width, strings.Repeat("-", width))

	y++

	return y
}

func (c *Client) renderWaitingRoom(x, y, width, heigh int) int {
	c.emitStr(x, y, width, strings.Repeat("-", width))
	y++
	c.emitInTheMiddle(x, y, width, "WAITING ROOM:")
	y++

	for _, name := range c.state.waitingRoom {
		b := strings.Builder{}
		if name == c.state.myName {
			b.WriteString("[YOU:]")
		}
		b.WriteString(name)
		c.emitStr(x, y, width, b.String())
		y++
	}
	c.emitStr(x, y, width, strings.Repeat("-", width))
	y++

	return y
}

func (c *Client) emitInTheMiddle(x, y, width int, str string) {
	c.emitStr(x+width/2-len(str)/2, y, width, str)
}

func (c *Client) emitStr(x, y, width int, str string) {
	for i, char := range []rune(str) {
		if i >= width {
			break
		}

		c.screen.SetContent(x+i, y, char, nil, tcell.StyleDefault)
	}
}
