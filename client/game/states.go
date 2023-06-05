package game

import "soa_project/pkg/proto/mafia"

type GameState = int

const (
	StateRegister = iota
	StateWaiting
	StatePlaying
	StateKilling
	StateSearching
	StateVoting

	StateChilling
)

type TimeType int

const (
	TimeDay TimeType = iota
	TimeNight
)

func GetTimeName(time TimeType) string {
	switch time {
	case TimeDay:
		return "DAY"
	case TimeNight:
		return "NIGHT"
	}

	return "ERROR??"
}

func GetAliveStateName(state mafia.MafiaState) string {
	switch state {
	case mafia.MafiaState_ALIVE:
		return "ALIVE"
	case mafia.MafiaState_DEAD:
		return "DEAD"
	}

	return "ERROR??"
}
