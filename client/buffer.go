package main

import "soa_project/utils/slices"

type CircularBuffer struct {
	buffer []string
	size   int
	head   int
	tail   int
	count  int
}

func NewCircularBuffer(size int) *CircularBuffer {
	return &CircularBuffer{
		buffer: make([]string, size),
		size:   size,
		head:   0,
		tail:   0,
		count:  0,
	}
}

func (cb *CircularBuffer) Add(command string) {
	cb.buffer[cb.head] = command
	cb.head = (cb.head + 1) % cb.size
	if cb.count < cb.size {
		cb.count++
	} else {
		cb.tail = (cb.tail + 1) % cb.size
	}
}

func (cb *CircularBuffer) Clear() {
	cb.count = 0
	cb.head = 0
	cb.tail = 0
}

func (cb *CircularBuffer) Get() []string {
	commands := make([]string, cb.count)
	if cb.count < cb.size {
		copy(commands, cb.buffer[:cb.count])
	} else {
		copy(commands, cb.buffer[cb.tail:])
		copy(commands[cb.size-cb.tail:], cb.buffer[:cb.head])
	}
	return commands
}

func (cb *CircularBuffer) GetReversed() []string {
	return slices.Reverse(cb.Get())
}
