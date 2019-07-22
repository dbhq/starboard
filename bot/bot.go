package bot

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GitbookIO/syncgroup"

	"github.com/dbhq/starboard/bot/util"

	"github.com/dbhq/discordgo"
	"github.com/dbhq/starboard/bot/commandler"
	"github.com/dbhq/starboard/bot/localization"
	"github.com/dbhq/starboard/bot/settings"
	"github.com/dbhq/starboard/bot/tables"
	"github.com/go-pg/pg"
	"github.com/go-pg/pg/orm"
	"github.com/jonas747/dshardmanager"
	"github.com/patrickmn/go-cache"
)

const (
	settingPrefix                = "prefix"
	settingLanguage              = "language"
	settingMinimum               = "minimum"
	settingSelfStar              = "self_star"
	settingSelfStarWarning       = "self_star_warning"
	settingEmoji                 = "emoji"
	settingChannel               = "channel"
	settingNSFWChannel           = "nsfw_channel"
	settingMinimal               = "minimal"
	settingRemoveBotStars        = "remove_bot_stars"
	settingSaveDeletedMessages   = "save_deleted_messages"
	settingBlockMode             = "block_mode"
	settingRandomStarProbability = "random_star_probability"

	settingNone = "none"
)

const starEmoji = "⭐"

// Bot represents a starboard instance
type Bot struct {
	PG        *pg.DB
	Cache     *cache.Cache
	Manager   *dshardmanager.Manager
	Locales   *localization.Locales
	Settings  *settings.Settings
	StartTime time.Time

	expectedGuilds map[*discordgo.Session]int
	mutexGroup     *syncgroup.MutexGroup
	opts           *Options
}

// DiscordList represents a Discord Bot list
type DiscordList struct {
	Authorization string
	URL           func(id string) string
	Serialize     func(shardCount, guildCount int) ([]byte, error)
}

// Options represents the options for creating a starboard instance
type Options struct {
	Prefix       string
	Token        string
	Locales      string
	OwnerID      string
	Mode         string
	DiscordLists []DiscordList

	Guild            string
	GuildLogChannel  string
	MemberLogChannel string
}

// New creates a starboard instance
func New(opts *Options, pgOpts *pg.Options) (err error) {
	b := &Bot{
		PG:        pg.Connect(pgOpts),
		Cache:     cache.New(time.Minute*20, time.Hour),
		StartTime: time.Now(),

		expectedGuilds: make(map[*discordgo.Session]int),
		mutexGroup:     syncgroup.NewMutexGroup(),
		opts:           opts,
	}

	b.Locales, err = localization.New(b.opts.Locales)
	if err != nil {
		return
	}

	b.Settings, err = settings.New(b.PG, map[string]interface{}{
		settingPrefix:          b.opts.Prefix,
		settingLanguage:        "en-US",
		settingMinimum:         1,
		settingSelfStar:        false,
		settingSelfStarWarning: false,
		settingEmoji: &util.Emoji{
			Name:    "star",
			Unicode: starEmoji,
		},
		settingChannel:               settingNone,
		settingNSFWChannel:           settingNone,
		settingMinimal:               false,
		settingRemoveBotStars:        true,
		settingSaveDeletedMessages:   false,
		settingBlockMode:             "blacklist",
		settingRandomStarProbability: float64(0),
	})
	if err != nil {
		return
	}

	err = b.createTables((*tables.Message)(nil), (*tables.Reaction)(nil), (*tables.Block)(nil))
	if err != nil {
		return
	}

	b.Manager = dshardmanager.New(b.opts.Token)
	b.Manager.SessionFunc = dshardmanager.SessionFunc(func(token string) (s *discordgo.Session, err error) {
		s, err = discordgo.New("Bot " + token)
		if err != nil {
			return nil, err
		}

		s.State.TrackEmojis = false
		s.State.TrackPresences = false
		s.State.TrackVoice = false

		s.AddHandler(b.ready)

		if b.opts.GuildLogChannel != "" {
			s.AddHandler(b.guildCreate)
			s.AddHandler(b.guildDelete)
		}

		if b.opts.Guild != "" && b.opts.MemberLogChannel != "" {
			s.AddHandler(b.guildMemberAdd)
			s.AddHandler(b.guildMemberRemove)
		}

		s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
			tags := map[string]string{"event": "MESSAGE_CREATE"}
			b.reportError(b.messageCreate(s, m), tags)
		})

		s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageUpdate) {
			tags := map[string]string{"event": "MESSAGE_UPDATE"}
			b.reportError(b.messageUpdate(s, m), tags)
		})

		s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageDelete) {
			tags := map[string]string{"event": "MESSAGE_DELETE"}
			b.reportError(b.messageDelete(s, m), tags)
		})

		s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageDeleteBulk) {
			tags := map[string]string{"event": "MESSAGE_DELETE_BULK"}
			b.reportError(b.messageDeleteBulk(s, m), tags)
		})

		s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageReactionAdd) {
			tags := map[string]string{"event": "MESSAGE_REACTION_ADD"}
			b.reportError(b.messageReactionAdd(s, m), tags)
		})

		s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageReactionRemove) {
			tags := map[string]string{"event": "MESSAGE_REACTION_REMOVE"}
			b.reportError(b.messageReactionRemove(s, m), tags)
		})

		s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageReactionRemoveAll) {
			tags := map[string]string{"event": "MESSAGE_REACTION_REMOVE_ALL"}
			b.reportError(b.messageReactionRemoveAll(s, m), tags)
		})

		c := commandler.New(s, b.Locales, b.Settings)
		c.OwnerID = b.opts.OwnerID
		b.registerCommands(c)

		if b.prod() {
			c.SetOnError(func(ctx *commandler.Context, err error, panicked bool) {
				b.reportError(err, map[string]string{
					"command":  ctx.Command.Name,
					"args":     strings.Join(ctx.Args, " "),
					"panicked": strconv.FormatBool(panicked),
				})

				ctx.Say("error")
			})
		} else {
			c.SetOnError(func(ctx *commandler.Context, err error, panicked bool) {
				_, mErr := ctx.SayRaw(fmt.Sprintf("Nice error, dumbass\nPanicked: `%t`\nError:\n```\n%s\n```\nStack trace:\n```\n%s\n```", panicked, err.Error(), debug.Stack()[:1500]))
				if mErr != nil {
					fmt.Println(mErr)
				}
			})
		}

		return
	})

	err = b.Manager.Start()
	if err != nil {
		return
	}

	b.initStatPoster(time.Minute)
	return
}

func (b *Bot) initStatPoster(d time.Duration) {
	var shardCount, guildCount int

	for range time.NewTicker(d).C {
		newShardCount := len(b.Manager.Sessions)
		newGuildCount := 0

		for _, s := range b.Manager.Sessions {
			newGuildCount += len(s.State.Guilds)
		}

		if shardCount == newShardCount && guildCount == newGuildCount {
			continue
		}

		shardCount = newShardCount
		guildCount = newGuildCount

		for _, list := range b.opts.DiscordLists {
			if list.Authorization == "" {
				continue
			}

			url := list.URL(b.Manager.Sessions[0].State.User.ID)

			data, err := list.Serialize(shardCount, guildCount)
			if err != nil {
				b.reportError(err, map[string]string{"url": url})
				continue
			}

			req, err := http.NewRequest("POST", url, bytes.NewReader(data))
			if err != nil {
				b.reportError(err, map[string]string{"url": url})
				continue
			}

			req.Header.Set("Authorization", list.Authorization)
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if resp.StatusCode >= http.StatusBadRequest {
				err = errors.New(resp.Status)
			}

			if err != nil {
				b.reportError(err, map[string]string{"url": url})
			}
		}
	}
}

func (b *Bot) createTables(tables ...interface{}) (err error) {
	for _, t := range tables {
		if b.dev() {
			err = b.PG.DropTable(t, &orm.DropTableOptions{IfExists: true})
			if err != nil {
				return
			}
		}

		err = b.PG.CreateTable(t, &orm.CreateTableOptions{IfNotExists: true})
		if err != nil {
			return
		}
	}

	return
}

func (b *Bot) reportError(err error, tags map[string]string) {
	if err == nil {
		return
	}

	fmt.Printf("Reported error: %v\n", err)

	var names []string
	for name := range tags {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		fmt.Printf("%v: %v\n", name, tags[name])
	}
}

func (b *Bot) capturePanic(f func(), tags map[string]string) {
	defer func() {
		switch err := recover().(type) {
		case nil:
			return
		case error:
			b.reportError(err, tags)
		default:
			b.reportError(fmt.Errorf("%v", err), tags)
		}
	}()

	f()
}

func (b *Bot) dev() bool {
	return b.opts.Mode == "dev"
}

func (b *Bot) prod() bool {
	return b.opts.Mode == "prod"
}
