package main

import (
	"example.com/constant-test/internal/service"
	"fmt"
)

func main() {
	fmt.Println("Starting server...")
	err := service.DoWithRetry()
	if err != nil {
		fmt.Println("Error:", err)
	}
}
