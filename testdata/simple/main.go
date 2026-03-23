// Package main is a simple test module.
package main

import "fmt"

// Greet returns a greeting for the given name.
func Greet(name string) string {
	return fmt.Sprintf("hello, %s", name)
}

func helper() int {
	return 42
}

// MyType is a test struct.
type MyType struct {
	Value int
}

// String implements fmt.Stringer.
func (m MyType) String() string {
	return fmt.Sprintf("MyType{%d}", m.Value)
}

// MyInterface is a test interface.
type MyInterface interface {
	Do() error
}

// MyConst is a test constant.
const MyConst = 100

func main() {
	fmt.Println(Greet("world"))
	t := MyType{Value: MyConst}
	fmt.Println(t.String())
	helper()
}
