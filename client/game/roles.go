package game

import "soa_project/pkg/proto/mafia"

func GetRoleName(role mafia.MafiaRole) string {
	switch role {
	case mafia.MafiaRole_CIVILIAN:
		return "CIVIL"
	case mafia.MafiaRole_MAFIA:
		return "MAFIA"
	case mafia.MafiaRole_SHERIFF:
		return "SHERIFF"
	case mafia.MafiaRole_UNKNOWN:
		return "UNKNOWN"
	}

	return "ERROR??"
}
