package demo

import "errors"

// Café 😀 comes first.
var ErrBad = errors.New("Bad.")

func Pick(ok bool) int {
	if ok {
		return 1
	} else {
		return 2
	}
}

func set(p_value int) int { return p_value }

// T is a documented literal struct.
type T struct{ A int }

//go:noinline
func (T) Quiet() {}

// Pair returns two numbers.
func Pair() (int, int) { return 1, 2 }
