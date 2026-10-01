package main

import (
	"fmt"
	"log"
	"os"

	"github.com/bwmarrin/discordgo"
)

func main() {
	token := os.Getenv("TOKEN")
	fmt.Print(token)
	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal(err, sess)
	}

}
