package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

// slashCommands are registered on startup. Guild-scoped registration (when
// DISCORD_GUILD_ID is set) is instant; global registration can take up to an hour.
var slashCommands = []*discordgo.ApplicationCommand{
	{Name: "login", Description: "Sign in to iCollege again (use if a login timed out)"},
	{Name: "refresh", Description: "Fetch assignments now and send any due reminders"},
	{Name: "assignments", Description: "List current upcoming assignments"},
}

// runBot runs the always-on service: it polls on a ticker and handles slash commands.
func runBot(ctx context.Context, app *App) error {
	if app.cfg.BotToken == "" {
		return fmt.Errorf("bot mode needs DISCORD_TOKEN")
	}
	s, err := discordgo.New("Bot " + app.cfg.BotToken)
	if err != nil {
		return err
	}
	// Only interaction events are needed; DM 2FA replies come over REST, so the
	// privileged message-content intent isn't required.
	s.Identify.Intents = discordgo.IntentsGuilds

	bot := &bot{app: app, session: s}
	s.AddHandler(bot.onInteraction)

	if err := s.Open(); err != nil {
		return fmt.Errorf("opening gateway: %w", err)
	}
	defer s.Close()

	if err := bot.registerCommands(); err != nil {
		return fmt.Errorf("registering commands: %w", err)
	}
	// Presence: "Watching assignments".
	if err := s.UpdateWatchStatus(0, "assignments"); err != nil {
		log.Printf("setting presence: %v", err)
	}
	log.Printf("bot online as %s; polling every %s", s.State.User.Username, app.cfg.PollInterval)

	// First pass immediately, then on the ticker.
	bot.safePass(ctx)
	ticker := time.NewTicker(app.cfg.PollInterval)
	defer ticker.Stop()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-stop:
			log.Printf("shutting down")
			return nil
		case <-ticker.C:
			bot.safePass(ctx)
		}
	}
}

type bot struct {
	app     *App
	session *discordgo.Session
}

func (b *bot) registerCommands() error {
	appID := b.session.State.User.ID
	guildID := b.app.cfg.GuildID // "" => global
	for _, c := range slashCommands {
		if _, err := b.session.ApplicationCommandCreate(appID, guildID, c); err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
	}
	return nil
}

func (b *bot) safePass(ctx context.Context) {
	if err := b.app.Pass(ctx); err != nil {
		log.Printf("pass: %v", err)
	}
}

// onInteraction only serves the configured user; others get a polite refusal.
func (b *bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	if uid := interactionUserID(i); uid != b.app.cfg.DMUserID {
		respond(s, i, textEmbed("Not allowed", "This bot only answers its owner.", colorGrey))
		return
	}

	name := i.ApplicationCommandData().Name
	// login/refresh do slow work (Chrome, network); defer then follow up.
	deferResponse(s, i)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var e *discordgo.MessageEmbed
		switch name {
		case "login":
			e = textEmbed("iCollege sign-in", b.app.ForceLogin(ctx), colorBlue)
		case "refresh":
			if err := b.app.Pass(ctx); err != nil {
				e = textEmbed("🔄 Refresh failed", err.Error(), colorRed)
			} else {
				e = textEmbed("🔄 Refreshed", "Fetched the latest assignments and sent any due reminders.", colorGreen)
			}
		case "assignments":
			e = b.assignmentsEmbed()
		default:
			e = textEmbed("Unknown command", "That command isn't recognized.", colorGrey)
		}
		followup(s, i, e)
	}()
}

// assignmentsEmbed summarizes upcoming, incomplete work from the latest fetch.
func (b *bot) assignmentsEmbed() *discordgo.MessageEmbed {
	b.app.mu.Lock()
	sc := b.app.last
	b.app.mu.Unlock()
	if sc == nil || len(sc.Assignments) == 0 {
		return textEmbed("📚 Upcoming assignments", "No assignment data yet. Try `/refresh`.", colorGrey)
	}
	loc := b.app.cfg.Location
	now := time.Now()

	type row struct {
		due  time.Time
		text string
	}
	var rows []row
	for _, a := range sc.Assignments {
		due, err := a.DueTime(loc)
		if err != nil || !due.After(now) {
			continue
		}
		if a.Completed() && b.app.cfg.SkipCompleted {
			continue
		}
		mark := ""
		if a.Timed {
			mark = " ⏱"
		}
		rows = append(rows, row{due, fmt.Sprintf("**%s**%s\n<t:%d:F> · <t:%d:R>", a.Name, mark, due.Unix(), due.Unix())})
	}
	if len(rows) == 0 {
		return textEmbed("📚 Upcoming assignments", "Nothing upcoming and incomplete. 🎉", colorGreen)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].due.Before(rows[j].due) })
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = r.text
	}
	e := textEmbed(fmt.Sprintf("📚 Upcoming assignments (%d)", len(rows)), strings.Join(lines, "\n\n"), colorBlue)
	if sc.Course != "" {
		e.Footer = &discordgo.MessageEmbedFooter{Text: sc.Course}
	}
	return e
}

func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID // in a guild
	}
	if i.User != nil {
		return i.User.ID // in a DM
	}
	return ""
}

func textEmbed(title, desc string, color int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Title: title, Description: desc, Color: color}
}

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, e *discordgo.MessageEmbed) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{e}, Flags: discordgo.MessageFlagsEphemeral},
	})
}

func deferResponse(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})
}

func followup(s *discordgo.Session, i *discordgo.InteractionCreate, e *discordgo.MessageEmbed) {
	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Embeds: []*discordgo.MessageEmbed{e},
		Flags:  discordgo.MessageFlagsEphemeral,
	}); err != nil {
		log.Printf("followup: %v", err)
	}
}
