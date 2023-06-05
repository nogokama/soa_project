package slices

import "math/rand"

func Shuffle[T any](a []T) []T {
	for i := 0; i < len(a); i++ {
		j := rand.Intn(i + 1)
		a[i], a[j] = a[j], a[i]
	}

	return a
}

func Contains[T comparable](a []T, val T) bool {
	for _, el := range a {
		if el == val {
			return true
		}
	}

	return false
}

func Reverse[T any](a []T) []T {
	for i := 0; i < len(a)-i-1; i++ {
		a[i], a[len(a)-i-1] = a[len(a)-i-1], a[i]
	}

	return a
}
