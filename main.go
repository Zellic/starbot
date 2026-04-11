package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/bwmarrin/discordgo"
	_ "github.com/mattn/go-sqlite3"
)

const (
	defaultThreshold = 10
)

var command = &discordgo.ApplicationCommand{
	Name:        "starbot",
	Description: "Starbot commands",
	Options: []*discordgo.ApplicationCommandOption{
		{
			Name:        "check",
			Description: "Check starboard status for this channel",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		},
		{
			Name:        "starboard",
			Description: "Get or set the starboard channel",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:         discordgo.ApplicationCommandOptionChannel,
					Name:         "channel",
					Description:  "Starboard channel",
					Required:     false,
					ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText},
				},
			},
		},
		{
			Name:        "threshold",
			Description: "Get or set the starboard threshold",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "value",
					Description: "Starboard threshold",
					Required:    false,
					MinValue:    floatPtr(1),
				},
			},
		},
		{
			Name:        "addprivate",
			Description: "Add a private channel keyword",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "keyword",
					Description: "Private keyword",
					Required:    true,
				},
			},
		},
		{
			Name:        "removeprivate",
			Description: "Remove a private channel keyword",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "keyword",
					Description: "Private keyword",
					Required:    true,
				},
			},
		},
		{
			Name:        "listprivate",
			Description: "List private channel keywords",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		},
	},
}

type Config struct {
	GuildID   string
	ChannelID string
	Threshold int
	Privates  []string
}

type Store struct {
	db *sql.DB
}

func main() {
	token := strings.TrimSpace(os.Getenv("DISCORD_TOKEN"))
	if token == "" {
		log.Fatal("DISCORD_TOKEN is required")
	}

	db, err := sql.Open("sqlite3", "./bot.db")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	store := &Store{db: db}
	if err := store.Init(); err != nil {
		log.Fatalf("init db: %v", err)
	}

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("create discord session: %v", err)
	}

	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		if i.ApplicationCommandData().Name == command.Name {
			handleCommand(s, store, i)
		}
	})
	dg.AddHandler(func(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
		handleReactionAdd(s, store, r)
	})

	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsGuildMessageReactions

	if err := dg.Open(); err != nil {
		log.Fatalf("open discord session: %v", err)
	}
	defer dg.Close()

	if err := dg.UpdateStatusComplex(discordgo.UpdateStatusData{
		Status: string(discordgo.StatusInvisible),
	}); err != nil {
		log.Fatalf("set invisible status: %v", err)
	}

	appID := dg.State.User.ID
	_, err = dg.ApplicationCommandCreate(appID, "", command)
	if err != nil {
		log.Fatalf("register commands: %v", err)
	}

	log.Println("Bot is running")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
}

func (s *Store) Init() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS configs (
			guild_id TEXT PRIMARY KEY,
			channel_id TEXT,
			threshold INTEGER,
			privates TEXT
		);
	`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sents (
			message_id TEXT PRIMARY KEY
		);
	`)
	return err
}

func (s *Store) GetConfig(guildID string) (Config, error) {
	var cfg Config
	cfg.GuildID = guildID
	var privates string
	err := s.db.QueryRow(
		`SELECT channel_id, threshold, privates FROM configs WHERE guild_id = ?`,
		guildID,
	).Scan(&cfg.ChannelID, &cfg.Threshold, &privates)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			cfg.Threshold = defaultThreshold
			cfg.Privates = []string{}
			if err := s.UpsertConfig(cfg); err != nil {
				return Config{}, err
			}
			return cfg, nil
		}
		return Config{}, err
	}
	if cfg.Threshold == 0 {
		cfg.Threshold = defaultThreshold
	}
	if privates == "" {
		cfg.Privates = []string{}
	} else {
		cfg.Privates = strings.Split(privates, ",")
	}
	return cfg, nil
}

func (s *Store) UpsertConfig(cfg Config) error {
	privates := strings.Join(cfg.Privates, ",")
	_, err := s.db.Exec(`
		INSERT INTO configs (guild_id, channel_id, threshold, privates)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(guild_id) DO UPDATE SET
			channel_id = excluded.channel_id,
			threshold = excluded.threshold,
			privates = excluded.privates
	`, cfg.GuildID, cfg.ChannelID, cfg.Threshold, privates)
	return err
}

func (s *Store) HasSent(messageID string) (bool, error) {
	var existing string
	err := s.db.QueryRow(`SELECT message_id FROM sents WHERE message_id = ?`, messageID).Scan(&existing)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) MarkSent(messageID string) error {
	_, err := s.db.Exec(`INSERT INTO sents (message_id) VALUES (?)`, messageID)
	return err
}

func handleCommand(s *discordgo.Session, store *Store, i *discordgo.InteractionCreate) {
	if i.GuildID == "" {
		respond(s, i, "This command can only be used in a server.")
		return
	}

	cfg, err := store.GetConfig(i.GuildID)
	if err != nil {
		log.Printf("get config: %v", err)
		respond(s, i, "Failed to load configuration.")
		return
	}

	options := i.ApplicationCommandData().Options
	if len(options) == 0 {
		respond(s, i, "No subcommand provided.")
		return
	}

	sub := options[0]
	switch sub.Name {
	case "check":
		channel, err := s.Channel(i.ChannelID)
		if err != nil {
			log.Printf("get channel: %v", err)
			respond(s, i, "Failed to load channel.")
			return
		}
		if cfg.ChannelID == "" {
			respond(s, i, "Starboard is not set for this server.")
			return
		}
		if len(cfg.Privates) > 0 {
			if isPrivateChannel(channel.Name, cfg.Privates) {
				respond(s, i, "Starboard is set, but the name of this channel contains a private keyword.")
				return
			}
		}
		if channel.Type == discordgo.ChannelTypeGuildPrivateThread {
			respond(s, i, "Starboard is set, but this channel is a private thread.")
			return
		}
		respond(s, i, "Starboard is set and messages from this channel will be posted to the starboard.")
		return
	case "starboard":
		if len(sub.Options) == 0 {
			if cfg.ChannelID == "" {
				respond(s, i, "Starboard is not set.")
				return
			}
			respond(s, i, fmt.Sprintf("Starboard channel: <#%s>", cfg.ChannelID))
			return
		}
		if !checkAdmin(s, i) {
			return
		}
		channel := sub.Options[0].ChannelValue(s)
		if channel == nil {
			respond(s, i, "Please provide a valid channel.")
			return
		}
		cfg.ChannelID = channel.ID
		if err := store.UpsertConfig(cfg); err != nil {
			log.Printf("update config: %v", err)
			respond(s, i, "Failed to update configuration.")
			return
		}
		respond(s, i, fmt.Sprintf("Starboard channel set to <#%s>.", channel.ID))
	case "threshold":
		if len(sub.Options) == 0 {
			respond(s, i, fmt.Sprintf("Starboard threshold: %d", cfg.Threshold))
			return
		}
		if !checkAdmin(s, i) {
			return
		}
		value := int(sub.Options[0].IntValue())
		if value <= 0 {
			respond(s, i, "Threshold must be a positive integer.")
			return
		}
		cfg.Threshold = value
		if err := store.UpsertConfig(cfg); err != nil {
			log.Printf("update config: %v", err)
			respond(s, i, "Failed to update configuration.")
			return
		}
		respond(s, i, fmt.Sprintf("Starboard threshold set to %d.", value))
	case "addprivate":
		if !checkAdmin(s, i) {
			return
		}
		if len(sub.Options) == 0 {
			respond(s, i, "Please provide a private keyword.")
			return
		}
		keyword := strings.TrimSpace(strings.ToLower(sub.Options[0].StringValue()))
		if !validatePrivateKeyword(keyword) {
			respond(s, i, "Invalid private keyword.")
			return
		}
		if slices.Contains(cfg.Privates, keyword) {
			respond(s, i, fmt.Sprintf("Private keyword already added. Current private keywords: %s", formatPrivateList(cfg.Privates)))
			return
		}
		cfg.Privates = append(cfg.Privates, keyword)
		if err := store.UpsertConfig(cfg); err != nil {
			log.Printf("update config: %v", err)
			respond(s, i, "Failed to update configuration.")
			return
		}
		respond(s, i, fmt.Sprintf("Private keyword added. Current private keywords: %s", formatPrivateList(cfg.Privates)))
	case "removeprivate":
		if !checkAdmin(s, i) {
			return
		}
		if len(sub.Options) == 0 {
			respond(s, i, "Please provide a private keyword.")
			return
		}
		keyword := strings.TrimSpace(strings.ToLower(sub.Options[0].StringValue()))
		if !validatePrivateKeyword(keyword) {
			respond(s, i, "Invalid private keyword.")
			return
		}
		if !slices.Contains(cfg.Privates, keyword) {
			respond(s, i, fmt.Sprintf("Private keyword does not exist. Current private keywords: %s", formatPrivateList(cfg.Privates)))
			return
		}
		cfg.Privates = slices.DeleteFunc(cfg.Privates, func(item string) bool {
			return item == keyword
		})
		if err := store.UpsertConfig(cfg); err != nil {
			log.Printf("update config: %v", err)
			respond(s, i, "Failed to update configuration.")
			return
		}
		respond(s, i, fmt.Sprintf("Private keyword removed. Current private keywords: %s", formatPrivateList(cfg.Privates)))
	case "listprivate":
		respond(s, i, fmt.Sprintf("Private keywords: %s", formatPrivateList(cfg.Privates)))
	default:
		respond(s, i, "Unknown command.")
	}
}

func handleReactionAdd(s *discordgo.Session, store *Store, r *discordgo.MessageReactionAdd) {
	if r.GuildID == "" {
		return
	}
	if r.Emoji.APIName() != "⭐" {
		return
	}

	cfg, err := store.GetConfig(r.GuildID)
	if err != nil {
		log.Printf("get config: %v", err)
		return
	}
	if cfg.ChannelID == "" {
		return
	}
	channel, err := s.Channel(r.ChannelID)
	if err != nil {
		log.Printf("get channel: %v", err)
		return
	}
	if len(cfg.Privates) > 0 {
		if isPrivateChannel(channel.Name, cfg.Privates) {
			return
		}
	}
	if channel.Type == discordgo.ChannelTypeGuildPrivateThread {
		return
	}

	sent, err := store.HasSent(r.MessageID)
	if err != nil {
		log.Printf("check sent: %v", err)
		return
	}
	if sent {
		return
	}

	msg, err := s.ChannelMessage(r.ChannelID, r.MessageID)
	if err != nil || msg == nil {
		return
	}

	if msg.Type != discordgo.MessageTypeDefault &&
		msg.Type != discordgo.MessageTypeReply &&
		msg.Type != discordgo.MessageTypeChatInputCommand &&
		msg.Type != discordgo.MessageTypeContextMenuCommand {
		return
	}

	count := getStarCount(msg)
	if count < cfg.Threshold {
		return
	}

	if err := store.MarkSent(r.MessageID); err != nil {
		return
	}
	failIfNotExists := false
	member, err := s.GuildMember(r.GuildID, msg.Author.ID)
	if err != nil {
		log.Printf("get member: %v", err)
		return
	}
	displayName := member.Nick
	if displayName == "" {
		displayName = msg.Author.DisplayName()
	}
	jumpLink := fmt.Sprintf("🌟 https://discord.com/channels/%s/%s/%s by **%s**:", r.GuildID, r.ChannelID, r.MessageID, displayName)

	_, err = s.ChannelMessageSend(cfg.ChannelID, jumpLink)
	if err != nil {
		log.Printf("send starboard: %v", err)
		return
	}
	_, err = s.ChannelMessageSendComplex(cfg.ChannelID, &discordgo.MessageSend{
		Reference: &discordgo.MessageReference{
			Type:            discordgo.MessageReferenceTypeForward,
			MessageID:       r.MessageID,
			ChannelID:       r.ChannelID,
			GuildID:         r.GuildID,
			FailIfNotExists: &failIfNotExists,
		},
	})
	if err != nil {
		log.Printf("forward message: %v", err)
		return
	}
}

func getStarCount(msg *discordgo.Message) int {
	for _, reaction := range msg.Reactions {
		if reaction.Emoji.APIName() == "⭐" {
			return reaction.Count
		}
	}
	return 0
}

func formatPrivateList(list []string) string {
	if len(list) == 0 {
		return "(empty)"
	}
	return strings.Join(list, ", ")
}

func validatePrivateKeyword(value string) bool {
	if value == "" {
		return false
	}
	if strings.Contains(value, ",") {
		return false
	}
	return true
}

func isPrivateChannel(name string, privates []string) bool {
	if name == "" || len(privates) == 0 {
		return false
	}
	lower := strings.ToLower(name)
	for _, keyword := range privates {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		if keyword == "" {
			continue
		}
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:         content,
			AllowedMentions: &discordgo.MessageAllowedMentions{},
			Flags:           discordgo.MessageFlagsEphemeral,
		},
	})
}

func checkAdmin(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	if i.Member.Permissions&discordgo.PermissionAdministrator == 0 {
		respond(s, i, "Administrator permission required.")
		return false
	}
	return true
}

func floatPtr(v float64) *float64 {
	return &v
}
