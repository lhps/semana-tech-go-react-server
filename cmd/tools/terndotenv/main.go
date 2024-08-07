package main

import (
	"errors"
	"os/exec"

	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		panic(errors.New("Error loading .env file" + err.Error()))
	}

	cmd := exec.Command(
		"tern",
		"migrate",
		"--migrations",
		"./internal/store/pgstore/migrations",
		"--config",
		"./internal/store/pgstore/migrations/tern.conf",
	)
	if err := cmd.Run(); err != nil {
		panic(err)
	}
}
