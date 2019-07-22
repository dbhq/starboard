package bot

import (
	"math/rand"
	"strconv"
	"time"

	"github.com/dbhq/starboard/bot/util"

	"github.com/bwmarrin/discordgo"
	"github.com/dbhq/starboard/bot/tables"
	"github.com/go-pg/pg"
	"github.com/jinzhu/inflection"
)

const expiryTime = time.Minute * 20
const gray = 0x2E3036

var styles = [...]struct{ max, color int }{
	{100, 0x6F29CE},
	{50, 0xFFB549},
	{10, 0xFFB13F},
	{0, 0xFFAC33},
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

func (b *Bot) getStarboard(s *discordgo.Session, channelID, guildID string) (starboard string) {
	setting := settingChannel
	c, err := s.State.Channel(channelID)
	if err != nil || c.NSFW {
		setting = settingNSFWChannel
	}

	starboard = b.Settings.GetString(guildID, setting)
	if starboard == settingNone {
		g, err := s.State.Guild(guildID)
		if err != nil {
			return
		}

		ch := findDefaultChannel(setting, s.State, g)
		if ch == nil {
			return
		}

		starboard = ch.ID
	}

	if starboard == "" {
		starboard = settingNone
	}

	return
}

func (b *Bot) generateEmbed(msg *tables.Message, count int) (embed *discordgo.MessageEmbed) {
	emoji := b.Settings.GetEmoji(msg.GuildID, settingEmoji)
	minimal := b.Settings.GetBool(msg.GuildID, settingMinimal)
	s := b.Locales.Language(b.Settings.GetString(msg.GuildID, settingLanguage))

	embed = &discordgo.MessageEmbed{
		Author: &discordgo.MessageEmbedAuthor{
			Name: s("message.content"),
			URL:  "https://discordapp.com/channels/" + msg.GuildID + "/" + msg.ChannelID + "/" + msg.ID,
		},
		Color:       gray,
		Description: msg.Content,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   s("message.author"),
				Value:  "<@" + msg.AuthorID + ">",
				Inline: true,
			},
			{
				Name:   s("message.channel"),
				Value:  "<#" + msg.ChannelID + ">",
				Inline: true,
			},
		},
	}

	if emoji.ID == "" {
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text: emoji.Unicode + " " + strconv.Itoa(count),
		}
	} else {
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text:    strconv.Itoa(count),
			IconURL: emoji.URL(),
		}
	}

	if !minimal {
		if count != 1 {
			emoji.Name = inflection.Plural(emoji.Name)
		}

		embed.Footer.Text += " " + emoji.Name
		embed.Timestamp = util.SnowflakeTimestamp(msg.ID).Format(time.RFC3339)

		for _, style := range styles {
			if count >= style.max {
				embed.Color = style.color
				break
			}
		}
	}

	if msg.Image != "" {
		embed.Image = &discordgo.MessageEmbedImage{
			URL: msg.Image,
		}
	}

	return
}

func (b *Bot) getMessage(s *discordgo.Session, id, channel string) (msg *tables.Message, err error) {
	key := "messages:" + id
	res, found := b.Cache.Get(key)
	if !found {
		return nil, err
	}

	data := res.(*tables.Message)

	if res != "" {
		m, err := s.ChannelMessage(channel, id)
		if err != nil {
			return nil, err
		}

		c, err := s.State.Channel(m.ChannelID)
		if err != nil {
			return nil, err
		}

		msg = &tables.Message{
			ID:        id,
			AuthorID:  m.Author.ID,
			ChannelID: m.ChannelID,
			GuildID:   c.GuildID,

			Content: util.GetContent(m),
			Image:   util.GetImage(m),
		}

		completeCount, err := b.countStars(msg, true)
		if err != nil {
			return nil, err
		}

		emoji := b.Settings.GetEmoji(m.GuildID, settingEmoji)
		for _, r := range m.Reactions {
			if r.Emoji.ID == "" {
				if r.Emoji.Name != emoji.Unicode {
					continue
				}
			} else if r.Emoji.ID != emoji.ID {
				continue
			}

			if completeCount == r.Count {
				break
			}

			_, err = b.PG.Model((*tables.Reaction)(nil)).Where("message_id = ?", m.ID).Delete()
			if err != nil && err != pg.ErrNoRows {
				return nil, err
			}

			reactions := make([]tables.Reaction, 0)

			for len(reactions) < r.Count {
				users, err := s.MessageReactions(m.ChannelID, m.ID, r.Emoji.APIName(), 100)
				if err != nil {
					return nil, err
				}

				for _, u := range users {
					reactions = append(reactions, tables.Reaction{
						Bot:       u.Bot,
						UserID:    u.ID,
						MessageID: m.ID,
					})
				}
			}

			if len(reactions) != 0 {
				_, err = b.PG.Model(&reactions).OnConflict("DO NOTHING").Insert()
				if err != nil {
					return nil, err
				}
			}

			break
		}

		go b.cacheMessage(m)
	} else {
		msg = &tables.Message{
			ID:        id,
			AuthorID:  data.AuthorID,
			ChannelID: channel,
			GuildID:   data.GuildID,

			Content: data.Content,
			Image: data.Image,
		}
	}

	return
}

func (b *Bot) cacheMessage(m *discordgo.Message) {
	key := "messages:" + m.ID

	b.Cache.Set(key, &tables.Message{
		AuthorID: m.Author.ID,
		ChannelID: m.ChannelID,
		GuildID: m.GuildID,
		Content: util.GetContent(m),
		Image: util.GetImage(m),
	}, expiryTime)
}

func (b *Bot) createMessage(s *discordgo.Session, m *tables.Message) (err error) {
	m, err = b.getMessage(s, m.ID, m.ChannelID)
	if err != nil {
		return
	}

	c, _ := b.PG.
		Model((*tables.Block)(nil)).
		Where("guild_id = ?", m.GuildID).
		Where("type = 'user' AND id = ?", m.AuthorID).
		WhereOr("type = 'channel' AND id = ?", m.ChannelID).
		Count()

	switch b.Settings.GetString(m.GuildID, settingBlockMode) {
	case "blacklist":
		if c != 0 {
			return
		}

	case "whitelist":
		if c == 0 {
			return
		}
	}

	starboard := b.getStarboard(s, m.ChannelID, m.GuildID)
	if starboard == settingNone {
		return
	}

	count, err := b.countStars(m, false)
	if err != nil {
		return
	}

	if count < b.Settings.GetInt(m.GuildID, settingMinimum) {
		return
	}

	sent, err := s.ChannelMessageSendEmbed(starboard, b.generateEmbed(m, count))
	if err != nil {
		return
	}

	m.SentID = sent.ID
	err = b.PG.Insert(m)
	return
}

func (b *Bot) countStars(m *tables.Message, raw bool) (int, error) {
	q := b.PG.Model((*tables.Reaction)(nil)).Where("message_id = ?", m.ID)

	if !raw {
		if !b.Settings.GetBool(m.GuildID, settingSelfStar) {
			q = q.Where("user_id != ?", m.AuthorID)
		}

		if b.Settings.GetBool(m.GuildID, settingRemoveBotStars) {
			q = q.Where("bot = FALSE")
		}
	}

	return q.Count()
}

func (b *Bot) updateMessage(s *discordgo.Session, m *tables.Message) (err error) {
	err = b.PG.Select(m)
	if err != nil {
		if err == pg.ErrNoRows {
			return b.createMessage(s, m)
		}

		return
	}

	count, err := b.countStars(m, false)
	if err != nil {
		return
	}

	starboard := b.getStarboard(s, m.ChannelID, m.GuildID)
	if starboard == settingNone {
		return
	}

	if count < b.Settings.GetInt(m.GuildID, settingMinimum) {
		go s.ChannelMessageDelete(starboard, m.SentID)
		go b.PG.Delete(m)
		return
	}

	embed := b.generateEmbed(m, count)
	_, err = s.ChannelMessageEditEmbed(starboard, m.SentID, embed)
	if err == nil {
		return
	}

	if rErr, ok := err.(*discordgo.RESTError); ok && rErr.Message != nil {
		if rErr.Message.Code != discordgo.ErrCodeUnknownMessage {
			return
		}

		sent, err := s.ChannelMessageSendEmbed(starboard, embed)
		if err != nil {
			return err
		}

		_, err = b.PG.Model(&tables.Message{ID: m.ID, SentID: sent.ID}).WherePK().UpdateNotNull()
		return err
	}

	return
}
