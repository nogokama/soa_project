package main

import (
	"fmt"
	"log"
	"net"
	"soa_project/pkg/proto/mafia"
	"soa_project/server/utils/slices"
	"sync"
	"time"

	"google.golang.org/grpc"
)

const (
	startGameThreshold = 4
)

type gameSession struct {
	players map[string]mafia.MafiaRole

	mu sync.Mutex
}

func NewGameSession(names []string) *gameSession {

	roles := []mafia.MafiaRole{mafia.MafiaRole_CIVILIAN, mafia.MafiaRole_CIVILIAN, mafia.MafiaRole_MAFIA, mafia.MafiaRole_SHERIFF}
	roles = slices.Shuffle(roles)

	players := make(map[string]mafia.MafiaRole)

	if len(names) != startGameThreshold {
		log.Fatalln("wrong number of players")
	}

	for i := 0; i < len(roles); i++ {
		players[names[i]] = roles[i]
	}

	return &gameSession{
		players: players,

		mu: sync.Mutex{},
	}
}

type server struct {
	mafia.UnimplementedMafiaServer

	streams map[string]chan *mafia.Event

	games map[string]int

	sessions      []*gameSession
	newGameBuffer []string
	mu            sync.Mutex
}

func NewServer() *server {
	return &server{
		streams:  make(map[string]chan *mafia.Event),
		mu:       sync.Mutex{},
		sessions: []*gameSession{},
		games:    make(map[string]int),
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

	for name, ch := range s.streams {
		ch <- &mafia.Event{
			Data: &mafia.Event_Connect{Connect: &mafia.PersonEvent{
				PersonName: clientName,
			}},
		}

		if slices.Contains(s.newGameBuffer, name) {
			channel <- &mafia.Event{Data: &mafia.Event_Connect{Connect: &mafia.PersonEvent{PersonName: name}}}
		}
	}

	s.streams[clientName] = channel
	s.newGameBuffer = append(s.newGameBuffer, clientName)

	if s.shouldStartNewGame() {
		s.startNewGame(s.newGameBuffer)
		s.newGameBuffer = []string{}
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

func (s *server) shouldStartNewGame() bool {
	actualWaiting := []string{}
	for _, name := range s.newGameBuffer {
		_, ok := s.streams[name]
		if ok {
			actualWaiting = append(actualWaiting, name)
		}
	}

	s.newGameBuffer = actualWaiting

	return len(s.newGameBuffer) == startGameThreshold
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
		}}}

		switch session.players[name] {
		case mafia.MafiaRole_MAFIA:
			s.streams[name] <- &mafia.Event{Data: &mafia.Event_AskKill{}}
		case mafia.MafiaRole_SHERIFF:
			s.streams[name] <- &mafia.Event{Data: &mafia.Event_AskSearch{}}
		}
	}
}

func (s *server) ProcessDisconnect(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.streams, name)

	for _, ch := range s.streams {
		ch <- &mafia.Event{
			Data: &mafia.Event_Disconnect{Disconnect: &mafia.PersonEvent{
				PersonName: name,
			}},
		}
	}
}

func main() {
	lis, err := net.Listen("tcp", ":9000")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	srv := grpc.NewServer()

	s := NewServer()
	s.OnStart()

	mafia.RegisterMafiaServer(srv, s)

	log.Println("Start listening...")

	log.Fatalln(srv.Serve(lis))
}
