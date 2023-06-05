package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"soa_project/client/game"
	"soa_project/pkg/proto/mafia"
	"soa_project/server/utils/algo"
	"sort"
	"strconv"
	"strings"

	"github.com/gdamore/tcell"

	mapset "github.com/deckarep/golang-set/v2"
)

const (
	cursorSymbol = '\u2588'
	cmdPrefix    = "/>"

	enterNamePrefix     = "[ENTER YOUR NAME:]"
	waitForOthersPrefix = "[WAIT FOR OTHER PLAYERS]"

	shouldKillPrefix   = "CHOOSE PLAYER TO KILL"
	shouldSearchPrefix = "CHOOSE PLAYER TO CHECK"
	shouldVotePrefix   = "CHOOSE PLAYER TO VOTE"

	successWaitPrefix = "[SUCCESS, WAIT OTHERS]"

	chillingPrefix = "[YOU ARE DEAD, WAIT OR EXIT]"
)

var (
	validateError = errors.New("validate")
)

type Client struct {
	state clientState

	grpcClient       mafia.MafiaClient
	gameEventsChan   chan *mafia.Event
	gameEventsStream mafia.Mafia_RegisterClient

	screen           tcell.Screen
	screenEventsChan chan tcell.Event

	connectionFailedChan chan int
}

type clientState struct {
	myName string

	input       string
	inputPrefix string
	inputError  string

	gameState     game.GameState
	updatesBuffer *CircularBuffer

	commandsBuffer *CircularBuffer

	waitingRoom []string

	players map[string]mafia.MafiaRole
	states  map[string]mafia.MafiaState

	gameTime game.TimeType
	dayNum   int

	availableCommands mapset.Set[string]
}

func NewClient(screen tcell.Screen, cli mafia.MafiaClient) *Client {
	return &Client{
		state: clientState{
			input:             "",
			gameState:         game.StateRegister,
			updatesBuffer:     NewCircularBuffer(10),
			commandsBuffer:    NewCircularBuffer(10),
			waitingRoom:       []string{},
			players:           make(map[string]mafia.MafiaRole),
			states:            make(map[string]mafia.MafiaState),
			availableCommands: mapset.NewSet(game.ExitCommand, game.AutoCommand),
		},
		screen:               screen,
		connectionFailedChan: make(chan int),
		grpcClient:           cli,
		gameEventsChan:       make(chan *mafia.Event),
		screenEventsChan:     make(chan tcell.Event),
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
		case <-c.connectionFailedChan:
			c.handleConnectionFailed()
		}
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
		c.onAskKill()
	case *mafia.Event_AskSearch:
		c.onAskSearch()
	case *mafia.Event_AskVote:
		c.onAskVote()
	case *mafia.Event_DayStarted:
		c.onDayStarted(event.DayStarted)
	default:
		log.Fatalln("unexpected")
	}

	c.reRenderScreen()
}

func (c *Client) onDayStarted(day *mafia.DayStarted) {
	c.state.dayNum = int(day.GetDayNum())

	c.state.updatesBuffer.Add(fmt.Sprintf("[SYSTEM]: DAY %d started", c.state.dayNum))

	if day.GetKilledVictim() == c.state.myName {
		c.state.updatesBuffer.Add(fmt.Sprintf("[SYSTEM]: YOU ARE KILLED :("))
		c.state.gameState = game.StateChilling
	} else {
		c.state.updatesBuffer.Add(fmt.Sprintf("[SYSTEM]: player %s was KILLED :(", day.GetKilledVictim()))
	}

	c.state.states[day.GetKilledVictim()] = mafia.MafiaState_DEAD
}

func (c *Client) onAskKill() {
	c.state.gameState = game.StateKilling
	c.state.inputPrefix = fmt.Sprintf("[%s %d-%d]", shouldKillPrefix, 1, len(c.state.players))
}

func (c *Client) onAskSearch() {
	c.state.gameState = game.StateSearching
	c.state.inputPrefix = fmt.Sprintf("[%s %d-%d]", shouldSearchPrefix, 1, len(c.state.players))
}

func (c *Client) onAskVote() {
	c.state.gameState = game.StateVoting
	c.state.inputPrefix = fmt.Sprintf("[%s %d-%d]", shouldVotePrefix, 1, len(c.state.players))
}

func (c *Client) onGameStarted(gs *mafia.GameStarted) {
	c.state.updatesBuffer.Clear()
	c.state.updatesBuffer.Add("[SYSTEM]: GAME STARTED")

	for _, name := range gs.GetPlayers() {
		c.state.players[name] = mafia.MafiaRole_UNKNOWN
		c.state.states[name] = mafia.MafiaState_ALIVE
	}

	c.state.players[c.state.myName] = gs.GetRole()

	c.state.gameState = game.StatePlaying
	c.state.gameTime = game.TimeNight
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
		mayBeCommand := strings.TrimSpace(strings.ToLower(c.state.input))
		switch mayBeCommand {
		case game.ExitCommand:
			c.leaveGame()
		case game.AutoCommand:
			// TODO switch auto mode
		case game.FinishCommand:
			c.onFinishCommand()
		}

		if mayBeCommand != "" {
			c.state.inputError = ""
		}

		switch c.state.gameState {
		case game.StateRegister:
			c.onNameEntered()
		case game.StateKilling:
			c.onKillEntered()
		case game.StateSearching:
			c.onSearchEntered()
		case game.StateVoting:
			c.onVoteEntered()
		}

		c.state.commandsBuffer.Add(c.getFullInput())
		c.state.input = ""

	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(c.state.input) > 0 {
			c.state.input = c.state.input[:len(c.state.input)-1]
		}
	default:
		c.state.input += string(ev.Rune())
	}

	c.reRenderScreen()
}

func (c *Client) onFinishCommand() {

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
	c.state.input = ""
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
	go c.listenGameEvents()
}

func (c *Client) leaveGame() {
	c.screen.Fini()
	os.Exit(0)
}

func (c *Client) reRenderScreen() {
	screen := c.screen // TODO fix

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

	c.emitStr(screenWidth/2-5, 1, screenWidth, "MAFIA GAME")

	// Draw split-screen line
	for row := 4; row < screenHeight-1; row++ {
		screen.SetContent(screenWidth/2, row, '│', nil, borderStyle)
	}

	c.reRenderLeftScreen(1, 3, screenWidth/2-1, screenHeight-2)
	c.reRenderRightScreen(screenWidth/2+1, 3, screenWidth-(screenWidth/2-1)-3, screenHeight-2)

	screen.Show()
}

func (c *Client) reRenderLeftScreen(x, y, width, height int) {
	switch c.state.gameState {
	case game.StateRegister:
		c.state.inputPrefix = enterNamePrefix
	case game.StateWaiting:
		c.state.inputPrefix = waitForOthersPrefix
	case game.StateChilling:
		c.state.inputPrefix = chillingPrefix
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

	c.emitStr(x, y, width, b.String())
}

func (c *Client) getFullInput() string {
	return fmt.Sprintf("%s%s%s", c.state.inputPrefix, cmdPrefix, c.state.input)
}

func (c *Client) reRenderRightScreen(x, y, width, height int) {
	// choose right side

	switch c.state.gameState {
	case game.StateWaiting:
		y = c.renderWaitingRoom(x, y, width, height)
	case game.StatePlaying, game.StateKilling, game.StateSearching, game.StateVoting, game.StateChilling:
		y = c.renderGameRoom(x, y, width, height)
	}

	commands := c.state.updatesBuffer.Get()

	for i, command := range commands {
		c.emitStr(x, y+i, width, command)
	}
}

func (c *Client) renderGameRoom(x, y, width, height int) int {
	c.emitStr(x, y, width, strings.Repeat("-", width))
	y++
	c.emitInTheMiddle(x, y, width, "GAME ROOM:")
	y++

	show := map[string]string{}

	longest := 0
	for name, role := range c.state.players {
		b := strings.Builder{}
		if name == c.state.myName {
			b.WriteString("YOU, ")
		}
		b.WriteString(game.GetRoleName(role))
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
	for i, char := range str {
		if i >= width {
			break
		}

		c.screen.SetContent(x+i, y, char, nil, tcell.StyleDefault)
	}
}
