package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"soa_project/pkg/proto/mafia"

	"github.com/gdamore/tcell"
	"github.com/gdamore/tcell/encoding"
	"github.com/streadway/amqp"
	"google.golang.org/grpc"
)

func main() {
	server := flag.String("server", "mafia_server:10000", "Server address")
	flag.Parse()

	logFile, err := os.Create("debug.log")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetPrefix("[DEBUG] ")

	conn, err := grpc.Dial(*server, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		log.Fatalln(err)
	}
	defer conn.Close()

	grpcClient := mafia.NewMafiaClient(conn)

	chatConn, err := amqp.Dial("amqp://guest:guest@rabbitmq:5672/")
	if err != nil {
		log.Fatalf("Failed to create rabbitmq connectoin: %v. Wait for rabbit-mq to launch or restart the container", err)
	}

	log.SetOutput(logFile)
	encoding.Register()

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

	client := NewClient(screen, grpcClient, chatConn)

	client.Start()
}
