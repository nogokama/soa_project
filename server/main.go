package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"soa_project/pkg/proto/mafia"
	"soa_project/utils/slices"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"

	mapset "github.com/deckarep/golang-set/v2"
)

const (
	startGameThreshold = 4
)

var (
	requestError = errors.New("request error")
)

type gameSession struct {
	players map[string]mafia.MafiaRole
	states  map[string]mafia.MafiaState

	expectKillFrom   mapset.Set[string]
	expectSearch     bool
	expectVoteFrom   mapset.Set[string]
	expectFinishFrom mapset.Set[string]

	dayCounter int
	lastKilled string
	voting     map[string]int
}

func NewGameSession(names []string) *gameSession {

	roles := []mafia.MafiaRole{mafia.MafiaRole_CIVILIAN, mafia.MafiaRole_CIVILIAN, mafia.MafiaRole_MAFIA, mafia.MafiaRole_SHERIFF}
	roles = slices.Shuffle(roles)

	players := make(map[string]mafia.MafiaRole)
	states := make(map[string]mafia.MafiaState)

	if len(names) != startGameThreshold {
		log.Fatalln("wrong number of players")
	}

	mafias := mapset.NewSet[string]()

	for i := 0; i < len(roles); i++ {
		players[names[i]] = roles[i]
		if roles[i] == mafia.MafiaRole_MAFIA {
			mafias.Add(names[i])
		}
		states[names[i]] = mafia.MafiaState_ALIVE
	}

	return &gameSession{
		players: players,
		states:  states,

		expectKillFrom: mafias,
		expectSearch:   true,

		expectVoteFrom:   mapset.NewSet[string](),
		expectFinishFrom: mapset.NewSet[string](),

		dayCounter: 1,
		voting:     make(map[string]int),
	}
}

type server struct {
	mafia.UnimplementedMafiaServer

	streams map[string]chan *mafia.Event

	games map[string]int

	sessions      []*gameSession
	newGameBuffer mapset.Set[string]
	mu            sync.Mutex
}

func NewServer() *server {
	return &server{
		streams:       make(map[string]chan *mafia.Event),
		mu:            sync.Mutex{},
		sessions:      []*gameSession{},
		games:         make(map[string]int),
		newGameBuffer: mapset.NewSet[string](),
	}
}

func (s *server) OnStart() {
	go s.pingClients()
}

func (s *server) pingClients() {
	for range time.NewTicker(time.Second).C {
		s.mu.Lock()
		for _, ch := range s.streams {
			ch <- &mafia.Event{Data: &mafia.Event_Ping{Ping: &mafia.PingMessage{}}}
		}
		s.mu.Unlock()
	}
}

func (s *server) Register(request *mafia.RegisterRequest, newStream mafia.Mafia_RegisterServer) error {
	s.mu.Lock()

	clientName := request.GetName()

	_, ok := s.streams[clientName]

	log.Println("request: ")
	for key := range s.streams {
		log.Println(key)
	}

	if ok {
		s.mu.Unlock()
		log.Println("CUR: ", clientName, "new name required")
		return nil // another name required
	}

	// notify others:

	channel := make(chan *mafia.Event, 10000)
	channel <- &mafia.Event{Data: &mafia.Event_HelloMessage{HelloMessage: fmt.Sprintf("HI, %s!", clientName)}}
	s.streams[clientName] = channel

	for _, waiter := range s.newGameBuffer.ToSlice() {
		s.streams[waiter] <- &mafia.Event{
			Data: &mafia.Event_Connect{Connect: &mafia.PersonEvent{
				PersonName: clientName,
			}},
		}

		channel <- &mafia.Event{Data: &mafia.Event_Connect{Connect: &mafia.PersonEvent{PersonName: waiter}}}
	}

	s.newGameBuffer.Add(clientName)

	if s.shouldStartNewGame() {
		s.startNewGame(s.newGameBuffer.ToSlice())
		s.newGameBuffer = mapset.NewSet[string]()
	}

	s.mu.Unlock()

	for {
		ev := <-channel

		if err := newStream.Send(ev); err != nil {
			s.ProcessDisconnect(clientName)
			return nil
		}
	}
}

func (s *server) Vote(ctx context.Context, req *mafia.GameRequest) (*mafia.GameResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, resp, err := s.actionPrepare(req)
	if err != nil {
		return resp, nil
	}

	session.expectVoteFrom.Remove(req.GetName())
	session.voting[req.GetVictim()]++

	s.mayBeNextNight(session)

	return &mafia.GameResponse{Success: true}, nil
}

func (s *server) Kill(ctx context.Context, req *mafia.GameRequest) (*mafia.GameResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, resp, err := s.actionPrepare(req)
	if err != nil {
		return resp, nil
	}

	session.expectKillFrom.Remove(req.GetName())
	session.lastKilled = req.GetVictim()

	s.mayBeNextDay(session)

	return &mafia.GameResponse{Success: true}, nil
}

func (s *server) Search(ctx context.Context, req *mafia.GameRequest) (*mafia.GameResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, resp, err := s.actionPrepare(req)
	if err != nil {
		return resp, nil
	}

	role := session.players[req.GetVictim()]

	session.expectSearch = false

	s.mayBeNextDay(session)

	return &mafia.GameResponse{Success: true, Role: &role}, nil
}

func (s *server) FinishDay(ctx context.Context, req *mafia.FinishDayRequest) (*mafia.GameResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, err := s.getSessionByPlayer(req.GetName())
	if err != nil {
		reason := err.Error()
		return &mafia.GameResponse{Success: false, Reason: &reason}, nil
	}

	session.expectFinishFrom.Remove(req.GetName())

	s.mayBeNextNight(session)

	return &mafia.GameResponse{Success: true}, nil
}

func (s *server) JoinWaitingRoom(ctx context.Context, req *mafia.JoinRequest) (*mafia.JoinResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.streams[req.GetName()]
	if !ok {
		return nil, requestError
	}

	delete(s.games, req.GetName())

	for _, waiter := range s.newGameBuffer.ToSlice() {
		s.streams[waiter] <- &mafia.Event{
			Data: &mafia.Event_Connect{Connect: &mafia.PersonEvent{
				PersonName: req.GetName(),
			}},
		}
	}

	s.newGameBuffer.Add(req.GetName())

	if s.shouldStartNewGame() {
		s.startNewGame(s.newGameBuffer.ToSlice())
		s.newGameBuffer = mapset.NewSet[string]()
	}

	return &mafia.JoinResponse{Players: s.newGameBuffer.ToSlice()}, nil
}

func (s *server) actionPrepare(req *mafia.GameRequest) (*gameSession, *mafia.GameResponse, error) {
	session, err := s.getSessionByPlayer(req.GetName())
	if err != nil {
		reason := err.Error()
		return nil, &mafia.GameResponse{Success: false, Reason: &reason}, requestError
	}

	victimState, ok := session.states[req.GetVictim()]
	if !ok {
		reason := "VICTIM IS NOT IN THE GAME"
		return nil, &mafia.GameResponse{Success: false, Reason: &reason}, requestError
	}

	if victimState != mafia.MafiaState_ALIVE {
		reason := fmt.Sprintf("VICTIM %s is not alive", req.GetVictim())
		return nil, &mafia.GameResponse{Success: false, Reason: &reason}, requestError
	}

	return session, &mafia.GameResponse{Success: true}, nil
}

func (s *server) mayBeNextNight(session *gameSession) {
	for _, name := range session.expectFinishFrom.ToSlice() {
		if session.expectVoteFrom.Contains(name) {
			return
		}
	}
	for _, name := range session.expectVoteFrom.ToSlice() {
		if session.expectFinishFrom.Contains(name) {
			return
		}
	}

	session.dayCounter++

	greater := false
	victim := ""
	maxVotes := 0

	for name, curVotes := range session.voting {
		if curVotes > maxVotes {
			greater = true
			victim = name
			maxVotes = curVotes
		} else if curVotes == maxVotes {
			greater = false
		}
	}

	msg := &mafia.VotingCompleted{}
	if greater {
		msg.KilledVictim = &victim
		session.states[victim] = mafia.MafiaState_DEAD
	}

	for name := range session.players {
		s.streams[name] <- &mafia.Event{Data: &mafia.Event_VotingCompleted{VotingCompleted: msg}}
	}

	if s.mayBeEndGame(session) {
		return
	}

	for name, role := range session.players {
		s.streams[name] <- &mafia.Event{Data: &mafia.Event_NightStarted{NightStarted: &mafia.NightStarted{
			NightNum: int32(session.dayCounter),
		}}}

		randomVictim := s.getRandomAliveVictim(session)
		switch role {
		case mafia.MafiaRole_MAFIA:
			s.streams[name] <- &mafia.Event{Data: &mafia.Event_AskKill{AskKill: &mafia.Ask{Default: randomVictim}}}
			session.expectKillFrom.Add(name)
		case mafia.MafiaRole_SHERIFF:
			s.streams[name] <- &mafia.Event{Data: &mafia.Event_AskSearch{AskSearch: &mafia.Ask{Default: randomVictim}}}
			session.expectSearch = true
		}
	}

}

func (s *server) mayBeNextDay(session *gameSession) {
	if session.expectSearch || len(session.expectKillFrom.ToSlice()) > 0 {
		return
	}

	session.states[session.lastKilled] = mafia.MafiaState_DEAD

	for name := range session.states {
		s.streams[name] <- &mafia.Event{Data: &mafia.Event_DayStarted{DayStarted: &mafia.DayStarted{
			DayNum:       int32(session.dayCounter),
			KilledVictim: session.lastKilled,
		}}}
	}

	if s.mayBeEndGame(session) {
		return
	}

	for name, state := range session.states {
		if state == mafia.MafiaState_ALIVE {
			s.streams[name] <- &mafia.Event{Data: &mafia.Event_AskVote{AskVote: &mafia.Ask{
				Default: s.getRandomAliveVictim(session),
			}}}

			session.expectFinishFrom.Add(name)
			session.expectVoteFrom.Add(name)
		}
	}

	session.lastKilled = ""
	session.voting = make(map[string]int)
}

func (s *server) mayBeEndGame(session *gameSession) bool {
	mafiasAlive := 0
	civilAlive := 0

	for name, state := range session.states {
		if state == mafia.MafiaState_ALIVE {
			switch session.players[name] {
			case mafia.MafiaRole_MAFIA:
				mafiasAlive++
			default:
				civilAlive++
			}
		}
	}

	if mafiasAlive == 0 {
		for name := range session.players {
			s.streams[name] <- &mafia.Event{Data: &mafia.Event_GameFinished{GameFinished: &mafia.GameFinished{
				WinRole: mafia.MafiaRole_CIVILIAN,
				Roles:   session.players,
			}}}
		}

		s.finishGameSession(session)
		return true
	}

	if mafiasAlive == civilAlive {
		for name := range session.players {
			s.streams[name] <- &mafia.Event{Data: &mafia.Event_GameFinished{GameFinished: &mafia.GameFinished{
				WinRole: mafia.MafiaRole_MAFIA,
				Roles:   session.players,
			}}}
		}

		s.finishGameSession(session)
		return true
	}

	return false
}

func (s *server) finishGameSession(session *gameSession) {
	// TODO
}

func (s *server) getSortedNames(session *gameSession) []string {
	names := []string{}
	for name := range session.players {
		names = append(names, name)
	}

	sort.Strings(names)
	return names
}

func (s *server) getRandomAliveVictim(session *gameSession) string {
	names := []string{}

	for name, state := range session.states {
		if state == mafia.MafiaState_ALIVE {
			names = append(names, name)
		}
	}

	names = slices.Shuffle(names)

	if len(names) == 0 {
		panic("get random names from len 0")
	}

	return names[0]
}

func (s *server) getSessionByPlayer(name string) (*gameSession, error) {
	idx, ok := s.games[name]
	if !ok {
		return nil, errors.New("Session not found")
	}

	return s.sessions[idx], nil
}

func (s *server) shouldStartNewGame() bool {
	actualWaiting := []string{}
	for _, name := range s.newGameBuffer.ToSlice() {
		_, ok := s.streams[name]
		if ok {
			actualWaiting = append(actualWaiting, name)
		}
	}

	s.newGameBuffer = mapset.NewSet(actualWaiting...)

	return len(actualWaiting) == startGameThreshold
}

func (s *server) startNewGame(n []string) {
	log.Println("Starting new game, players", n)

	names := make([]string, len(n))
	copy(names, n)

	session := NewGameSession(names)

	s.sessions = append(s.sessions, session)

	for name := range session.players {
		s.games[name] = len(s.sessions) - 1

		s.streams[name] <- &mafia.Event{Data: &mafia.Event_GameStarted{GameStarted: &mafia.GameStarted{
			Players: names,
			Role:    session.players[name],
			RoomId:  int32(len(s.sessions)),
		}}}
		s.streams[name] <- &mafia.Event{Data: &mafia.Event_NightStarted{NightStarted: &mafia.NightStarted{
			NightNum: int32(session.dayCounter),
		}}}

		randomVictim := s.getRandomAliveVictim(session)
		switch session.players[name] {
		case mafia.MafiaRole_MAFIA:
			s.streams[name] <- &mafia.Event{Data: &mafia.Event_AskKill{AskKill: &mafia.Ask{Default: randomVictim}}}
		case mafia.MafiaRole_SHERIFF:
			s.streams[name] <- &mafia.Event{Data: &mafia.Event_AskSearch{AskSearch: &mafia.Ask{Default: randomVictim}}}
		}
	}
}

func (s *server) ProcessDisconnect(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, name)

	if s.newGameBuffer.Contains(name) {
		s.newGameBuffer.Remove(name)
		for _, cName := range s.newGameBuffer.ToSlice() {
			s.streams[cName] <- &mafia.Event{
				Data: &mafia.Event_Disconnect{Disconnect: &mafia.PersonEvent{
					PersonName: name,
				}},
			}
		}
	} else if sessionNum, ok := s.games[name]; ok {
		for cName := range s.sessions[sessionNum].players {
			if cName != name {
				s.streams[cName] <- &mafia.Event{
					Data: &mafia.Event_Disconnect{Disconnect: &mafia.PersonEvent{
						PersonName: name,
					}},
				}
			}
		}
	}
}

func main() {
	lis, err := net.Listen("tcp", ":10000")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	seed := time.Now().UnixNano()
	rand.Seed(seed)

	srv := grpc.NewServer()

	s := NewServer()
	s.OnStart()

	mafia.RegisterMafiaServer(srv, s)

	log.Println("Start listening...")

	log.Fatalln(srv.Serve(lis))
}
