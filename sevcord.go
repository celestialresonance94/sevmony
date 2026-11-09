package sevcord

import (
	"fmt"
	"log"
	"os"
	"os/signal"
 "reflect"
 "sort"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
)

var Logger = log.Default()

// MiddlewareFunc accepts context and returns whether or not to continue
type MiddlewareFunc func(ctx Ctx, command string) (ok bool)

type MessageHandler func(ctx Ctx, content string)

type Sevcord struct {
	lock *sync.RWMutex

	dg             *discordgo.Session // Note: only use this to create cmds, give one provided with handlers for user
	middleware     []MiddlewareFunc
	messageHandler MessageHandler
	commands       map[string]SlashCommandObject
	buttonHandlers map[string]ButtonHandler
	selectHandlers map[string]SelectHandler
	modalHandlers  map[string]ModalHandler
}

func (s *Sevcord) RegisterSlashCommand(cmd SlashCommandObject) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.commands[cmd.name()] = cmd
}

func (s *Sevcord) SetMessageHandler(handler MessageHandler) {
	s.messageHandler = handler
}

func New(token string) (*Sevcord, error) {
	dg, err := discordgo.New("Bot " + strings.TrimSpace(token))
	if err != nil {
		return nil, err
	}
	dg.Identify.Intents = discordgo.IntentsNone
	return &Sevcord{
		lock:           &sync.RWMutex{},
		dg:             dg,
		middleware:     make([]MiddlewareFunc, 0),
		commands:       make(map[string]SlashCommandObject),
		buttonHandlers: make(map[string]ButtonHandler),
		selectHandlers: make(map[string]SelectHandler),
		modalHandlers:  make(map[string]ModalHandler),
	}, nil
}

// AddMiddleware adds middleware, a function that is run before every command handler is called. Middleware is run in the order it is added. Note that middleware is not run for message handlers
func (s *Sevcord) AddMiddleware(m MiddlewareFunc) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.middleware = append(s.middleware, m)
}

func (s *Sevcord) AddButtonHandler(id string, handler ButtonHandler) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.buttonHandlers[id] = handler
}

func (s *Sevcord) AddSelectHandler(id string, handler SelectHandler) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.selectHandlers[id] = handler
}

// Dg gets the global discordgo session. NOTE: Only use this to add handlers/intents, use the one provided with Ctx for anything else
func (s *Sevcord) Dg() *discordgo.Session {
	return s.dg
}

func (s *Sevcord) Listen() {
	s.lock.RLock()
	// Build commands
	cmds := make([]*discordgo.ApplicationCommand, 0, len(s.commands))
	for _, cmd := range s.commands {
		v := cmd.dg()
		dmPermission := false
		cmds = append(cmds, &discordgo.ApplicationCommand{
			Name:        v.Name,
			Description: v.Description,
			Options:     v.Options,
			Type:        discordgo.ChatApplicationCommand,

			DefaultMemberPermissions: cmd.permissions(),
			DMPermission:             &dmPermission,
		})
	}
	s.lock.RUnlock()

	// Serialize syncs for a guild: Ready and GuildCreate may both announce it.
    var syncMu sync.Mutex
    syncGuild := func(d *discordgo.Session, guildID string) {
        if guildID == "" || d.State == nil || d.State.User == nil { return }
        syncMu.Lock()
        defer syncMu.Unlock()
        appID := d.State.User.ID
        existing, err := d.ApplicationCommands(appID, guildID)
        if err != nil { Logger.Printf("Guild %s: list commands: %v", guildID, err); return }
        byName := make(map[string]*discordgo.ApplicationCommand, len(existing))
        for _, c := range existing { byName[c.Name] = c }
        desired := make(map[string]*discordgo.ApplicationCommand, len(cmds))
        for _, c := range cmds { desired[c.Name] = c }
        names := make([]string, 0, len(desired))
        for name := range desired { names = append(names, name) }
        sort.Strings(names)
        created, updated, unchanged, removed := 0, 0, 0, 0
        for _, name := range names {
            want := desired[name]
            old, exists := byName[name]
            if !exists {
                if _, err := d.ApplicationCommandCreate(appID, guildID, want); err != nil {
                    Logger.Printf("Guild %s: create /%s: %v", guildID, name, err)
                } else { created++ }
                continue
            }
            if old.Description == want.Description && old.Type == want.Type && reflect.DeepEqual(old.Options, want.Options) {
                unchanged++
                continue
            }
            if old.ID == "" { Logger.Printf("Guild %s: cannot edit /%s (missing ID)", guildID, name); continue }
            if _, err := d.ApplicationCommandEdit(appID, guildID, old.ID, want); err != nil {
                Logger.Printf("Guild %s: edit /%s: %v", guildID, name, err)
            } else { updated++ }
        }
        // This module owns the guild's application commands. Remove names no longer defined.
        for _, old := range existing {
            if _, exists := desired[old.Name]; exists { continue }
            if old.ID == "" { Logger.Printf("Guild %s: cannot delete /%s (missing ID)", guildID, old.Name); continue }
            if err := d.ApplicationCommandDelete(appID, guildID, old.ID); err != nil {
                Logger.Printf("Guild %s: delete /%s: %v", guildID, old.Name, err)
            } else { removed++ }
        }
        Logger.Printf("Guild %s: created=%d updated=%d unchanged=%d removed=%d", guildID, created, updated, unchanged, removed)
    }

    s.dg.AddHandler(s.interactionHandler)
    s.dg.AddHandler(func(d *discordgo.Session, r *discordgo.Ready) {
        Logger.Println("Bot Ready")
    })
    s.dg.AddHandler(func(d *discordgo.Session, g *discordgo.GuildCreate) {
        if g.Guild != nil { syncGuild(d, g.ID) }
    })
	if s.messageHandler != nil {
		s.dg.AddHandler(func(d *discordgo.Session, m *discordgo.MessageCreate) {
			if m.Author == nil {
				return // if the message was not successfully parsed, give up to avoid further damage
			}
			if m.Author.Bot {
				return
			}
			ctx := &MessageCtx{
				m: m.Message,
				d: d,
			}
			s.messageHandler(ctx, m.Content)
		})
	}

	// Open
	if s.messageHandler != nil {
		s.dg.Identify.Intents |= discordgo.IntentsGuildMessages
	}
	s.dg.Open()

	// Wait
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	fmt.Println("Gracefully shutting down...")

	// Close
	s.dg.Close()
}
