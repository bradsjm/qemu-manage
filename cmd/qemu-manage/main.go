package main

import (
	"context"
	"os"

	"qemu-manage/internal/cli"
)

func main() {
	app := cli.NewApp()
	os.Exit(app.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
