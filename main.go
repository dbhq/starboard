package main

import (
	"fmt"

	"github.com/BurntSushi/toml"
	"github.com/dbhq/starboard/bot"
)

var (
	c *config
)

type config struct {
	Prefix           string `toml:"prefix"`
	Token            string `toml:"token"`
	Locales          string `toml:"locales"`
	OwnerID          string `toml:"owner_id"`
	Guild            string `toml:"guild"`
	GuildLogChannel  string `toml:"guild_log_channel"`
	MemberLogChannel string `toml:"member_log_channel"`
}

func main() {
	if _, err := toml.DecodeFile("./starboard.config.toml", &c); err != nil {
		fmt.Println("error loading starboard config,", err)
		return
	}

	panic(bot.New(
		&bot.Options{
			Prefix:           c.Prefix,
			Token:            c.Token,
			Locales:          c.Locales,
			OwnerID:          c.OwnerID,
			Guild:            c.Guild,
			GuildLogChannel:  c.GuildLogChannel,
			MemberLogChannel: c.MemberLogChannel,
		},
	))
}
