package main

import (
	"fmt"
	"log"
	"os"
	"soa_project/pkg/proto/mafia"

	"github.com/gdamore/tcell"
	"github.com/gdamore/tcell/encoding"
	"google.golang.org/grpc"
)

func main() {
	encoding.Register()

	logFile, err := os.Create("debug.log")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	log.SetOutput(logFile)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetPrefix("[DEBUG] ")

	conn, err := grpc.Dial(":9000", grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		log.Fatalln(err)
	}
	defer conn.Close()

	grpcClient := mafia.NewMafiaClient(conn)

	screen, err := tcell.NewScreen()
	if err != nil {
		log.Fatalf("Failed to create screen: %v", err)
	}
	if err := screen.Init(); err != nil {
		log.Fatalf("Failed to initialize screen: %v", err)
	}
	defer screen.Fini()

	screen.Clear()
	screen.Show()

	client := NewClient(screen, grpcClient)

	client.Start()
}
