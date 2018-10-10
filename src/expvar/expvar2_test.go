package expvar

import "testing"

var sink string

func BenchmarkMapString(b *testing.B) {
	m := new(Map).Init()
	m.Add("red", 1)
	m.Add("green", 2)
	m.Add("blue", 3)
	m.Add("red1", 1)
	m.Add("green1", 2)
	m.Add("blue1", 3)
	m.Add("red2 very very very long name", 1)
	m.Add("green2 very very very long name", 2)
	m.Add("blue2 very very very long name", 3)

	for i := 0; i < b.N; i++ {
		sink = m.String()
	}
}
