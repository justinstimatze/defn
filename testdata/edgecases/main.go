package main

import "fmt"

// Grouped constants with iota.
const (
	Red = iota
	Green
	Blue
)

// Grouped constants without iota.
const (
	MaxSize = 100
	MinSize = 10
)

// Multi-name var declaration.
var x, y int

// Standalone var.
var Standalone = "hello"

// Grouped types.
type (
	MyInt    int
	MyString string
)

// Multiple init functions.
func init() {
	fmt.Println("init 1")
}

func init() {
	fmt.Println("init 2")
}

// Regular exported function.
func Hello() string {
	return fmt.Sprintf("hello %d %d", x, y)
}

// Method with receiver.
type Server struct{ Port int }

func (s *Server) Start() error {
	return nil
}

func main() {
	init := "not the init function" // local var named init
	_ = init
	Hello()
	s := &Server{Port: 8080}
	s.Start()
	fmt.Println(Red, Green, Blue, MaxSize, MinSize, Standalone)
}
