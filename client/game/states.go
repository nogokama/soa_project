package game

type GameState = int

const (
	StateRegister = iota
	StateWaiting
	StatePlaying
	StateKilling
	StateSearching
	StateVoting
)
