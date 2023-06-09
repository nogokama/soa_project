package game

import "soa_project/pkg/proto/mafia"

type AskQueue struct {
	queue []*mafia.Ask
}

func NewAskQueue() AskQueue {
	return AskQueue{queue: []*mafia.Ask{}}
}

func (q *AskQueue) Set(ask *mafia.Ask) {
	q.queue = []*mafia.Ask{ask}
}

func (q *AskQueue) Push(ask *mafia.Ask) {
	q.queue = append(q.queue, ask)
}

func (q *AskQueue) Pop() *mafia.Ask {
	if len(q.queue) == 0 {
		return nil
	}

	ans := q.queue[0]
	q.queue = q.queue[1:]
	return ans
}

func (q *AskQueue) Peek() *mafia.Ask {
	if len(q.queue) == 0 {
		return nil
	}

	return q.queue[0]
}

func (q *AskQueue) Reset() {
	q.queue = []*mafia.Ask{}
}
