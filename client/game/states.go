package game

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
