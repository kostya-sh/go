// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package expvar provides a standardized interface to public variables, such
// as operation counters in servers. It exposes these variables via HTTP at
// /debug/vars in JSON format.
//
// Operations to set or modify these public variables are atomic.
//
// In addition to adding the HTTP handler, this package registers the
// following variables:
//
//	cmdline   os.Args
//	memstats  runtime.Memstats
//
// The package is sometimes only imported for the side effect of
// registering its HTTP handler and the above variables. To use it
// this way, link this package into your program:
//	import _ "expvar"
//
package expvar

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Var is an abstract type for all exported variables.
type Var interface {
	// String returns a valid JSON value for the variable.
	// Types with String methods that do not return valid JSON
	// (such as time.Time) must not be used as a Var.
	String() string
}

// Int is a 64-bit integer variable that satisfies the Var interface.
type Int struct {
	i int64
}

func (v *Int) Value() int64 {
	return atomic.LoadInt64(&v.i)
}

func (v *Int) String() string {
	return strconv.FormatInt(atomic.LoadInt64(&v.i), 10)
}

func (v *Int) Add(delta int64) {
	atomic.AddInt64(&v.i, delta)
}

func (v *Int) Set(value int64) {
	atomic.StoreInt64(&v.i, value)
}

// Float is a 64-bit float variable that satisfies the Var interface.
type Float struct {
	f uint64
}

func (v *Float) Value() float64 {
	return math.Float64frombits(atomic.LoadUint64(&v.f))
}

func (v *Float) String() string {
	return strconv.FormatFloat(
		math.Float64frombits(atomic.LoadUint64(&v.f)), 'g', -1, 64)
}

// Add adds delta to v.
func (v *Float) Add(delta float64) {
	for {
		cur := atomic.LoadUint64(&v.f)
		curVal := math.Float64frombits(cur)
		nxtVal := curVal + delta
		nxt := math.Float64bits(nxtVal)
		if atomic.CompareAndSwapUint64(&v.f, cur, nxt) {
			return
		}
	}
}

// Set sets v to value.
func (v *Float) Set(value float64) {
	atomic.StoreUint64(&v.f, math.Float64bits(value))
}

type keys struct {
	sync.RWMutex
	keys       []string // sorted
	quotedKeys []string
}

func (v *keys) Len() int           { return len(v.keys) }
func (v *keys) Less(i, j int) bool { return v.keys[i] < v.keys[j] }
func (v *keys) Swap(i, j int)      { v.keys[i], v.quotedKeys[i] = v.keys[j], v.quotedKeys[j] }

func (k *keys) clear() {
	k.Lock()
	k.keys = k.keys[:0]
	k.quotedKeys = k.quotedKeys[:0]
	k.Unlock()
}

func (v *keys) add(key string) {
	v.Lock()
	v.keys = append(v.keys, key)
	v.quotedKeys = append(v.quotedKeys, strconv.Quote(key))
	sort.Sort(v)
	v.Unlock()
}

// Map is a string-to-Var map variable that satisfies the Var interface.
type Map struct {
	m    sync.Map // map[string]Var
	keys keys
}

// KeyValue represents a single entry in a Map.
type KeyValue struct {
	Key   string
	Value Var

	quotedKey string
}

func (v *Map) String() string {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	v.Do(func(kv KeyValue) {
		if !first {
			b.WriteString(", ")
		}
		b.WriteString(kv.quotedKey)
		b.WriteString(": ")
		b.WriteString(kv.Value.String())
		first = false
	})
	b.WriteByte('}')
	return b.String()
}

// Init removes all keys from the map.
func (v *Map) Init() *Map {
	v.keys.clear()
	v.m.Range(func(k, _ interface{}) bool {
		v.m.Delete(k)
		return true
	})
	return v
}

// addKey updates the sorted list of keys in v.keys.
func (v *Map) addKey(key string) {
	v.keys.add(key)
}

func (v *Map) Get(key string) Var {
	i, _ := v.m.Load(key)
	av, _ := i.(Var)
	return av
}

func (v *Map) Set(key string, av Var) {
	// Before we store the value, check to see whether the key is new. Try a Load
	// before LoadOrStore: LoadOrStore causes the key interface to escape even on
	// the Load path.
	if _, ok := v.m.Load(key); !ok {
		if _, dup := v.m.LoadOrStore(key, av); !dup {
			v.addKey(key)
			return
		}
	}

	v.m.Store(key, av)
}

// Add adds delta to the *Int value stored under the given map key.
func (v *Map) Add(key string, delta int64) {
	i, ok := v.m.Load(key)
	if !ok {
		var dup bool
		i, dup = v.m.LoadOrStore(key, new(Int))
		if !dup {
			v.addKey(key)
		}
	}

	// Add to Int; ignore otherwise.
	if iv, ok := i.(*Int); ok {
		iv.Add(delta)
	}
}

// AddFloat adds delta to the *Float value stored under the given map key.
func (v *Map) AddFloat(key string, delta float64) {
	i, ok := v.m.Load(key)
	if !ok {
		var dup bool
		i, dup = v.m.LoadOrStore(key, new(Float))
		if !dup {
			v.addKey(key)
		}
	}

	// Add to Float; ignore otherwise.
	if iv, ok := i.(*Float); ok {
		iv.Add(delta)
	}
}

// Deletes the given key from the map.
func (v *Map) Delete(key string) {
	v.keys.Lock()
	defer v.keys.Unlock()
	i := sort.SearchStrings(v.keys.keys, key)
	if i < len(v.keys.keys) && key == v.keys.keys[i] {
		v.keys.keys = append(v.keys.keys[:i], v.keys.keys[i+1:]...)
		v.keys.quotedKeys = append(v.keys.quotedKeys[:i], v.keys.quotedKeys[i+1:]...)
		v.m.Delete(key)
	}
}

// Do calls f for each entry in the map.
// The map is locked during the iteration,
// but existing entries may be concurrently updated.
func (v *Map) Do(f func(KeyValue)) {
	v.keys.RLock()
	defer v.keys.RUnlock()
	for j, k := range v.keys.keys {
		i, _ := v.m.Load(k)
		f(KeyValue{k, i.(Var), v.keys.quotedKeys[j]})
	}
}

// String is a string variable, and satisfies the Var interface.
type String struct {
	s atomic.Value // string
}

func (v *String) Value() string {
	p, _ := v.s.Load().(string)
	return p
}

// String implements the Var interface. To get the unquoted string
// use Value.
func (v *String) String() string {
	s := v.Value()
	b, _ := json.Marshal(s)
	return string(b)
}

func (v *String) Set(value string) {
	v.s.Store(value)
}

// Func implements Var by calling the function
// and formatting the returned value using JSON.
type Func func() interface{}

func (f Func) Value() interface{} {
	return f()
}

func (f Func) String() string {
	v, _ := json.Marshal(f())
	return string(v)
}

// All published variables.
var (
	vars      sync.Map // map[string]Var
	varKeysMu sync.RWMutex
	varKeys   []string // sorted
)

// Publish declares a named exported variable. This should be called from a
// package's init function when it creates its Vars. If the name is already
// registered then this will log.Panic.
func Publish(name string, v Var) {
	if _, dup := vars.LoadOrStore(name, v); dup {
		log.Panicln("Reuse of exported var name:", name)
	}
	varKeysMu.Lock()
	defer varKeysMu.Unlock()
	varKeys = append(varKeys, name)
	sort.Strings(varKeys)
}

// Get retrieves a named exported variable. It returns nil if the name has
// not been registered.
func Get(name string) Var {
	i, _ := vars.Load(name)
	v, _ := i.(Var)
	return v
}

// Convenience functions for creating new exported variables.

func NewInt(name string) *Int {
	v := new(Int)
	Publish(name, v)
	return v
}

func NewFloat(name string) *Float {
	v := new(Float)
	Publish(name, v)
	return v
}

func NewMap(name string) *Map {
	v := new(Map).Init()
	Publish(name, v)
	return v
}

func NewString(name string) *String {
	v := new(String)
	Publish(name, v)
	return v
}

// Do calls f for each exported variable.
// The global variable map is locked during the iteration,
// but existing entries may be concurrently updated.
func Do(f func(KeyValue)) {
	varKeysMu.RLock()
	defer varKeysMu.RUnlock()
	for _, k := range varKeys {
		val, _ := vars.Load(k)
		f(KeyValue{k, val.(Var), ""})
	}
}

func expvarHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write([]byte("{\n"))
	first := true
	keyBuf := make([]byte, 0, 64)
	Do(func(kv KeyValue) {
		if !first {
			w.Write([]byte(",\n"))
		}
		first = false
		keyBuf = strconv.AppendQuote(keyBuf[:0], kv.Key)
		w.Write(keyBuf)
		w.Write([]byte(": "))
		w.Write([]byte(kv.Value.String()))
	})
	w.Write([]byte("\n}\n"))
}

// Handler returns the expvar HTTP Handler.
//
// This is only needed to install the handler in a non-standard location.
func Handler() http.Handler {
	return http.HandlerFunc(expvarHandler)
}

func cmdline() interface{} {
	return os.Args
}

func memstats() interface{} {
	stats := new(runtime.MemStats)
	runtime.ReadMemStats(stats)
	return *stats
}

func init() {
	http.HandleFunc("/debug/vars", expvarHandler)
	Publish("cmdline", Func(cmdline))
	Publish("memstats", Func(memstats))
}
