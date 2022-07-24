package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"text/template"

	"github.com/bwmarrin/discordgo"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/connorkuehl/popple"
	poperrs "github.com/connorkuehl/popple/errors"
	"github.com/connorkuehl/popple/event"
)

var (
	token = os.Getenv("POPPLEBOT_DISCORD_TOKEN")

	amqpHost = os.Getenv("POPPLEBOT_AMQP_HOST")
	amqpPort = os.Getenv("POPPLEBOT_AMQP_PORT")
	amqpUser = os.Getenv("POPPLEBOT_AMQP_USER")
	amqpPass = os.Getenv("POPPLEBOT_AMQP_PASS")
)

var (
	templateLevels = template.Must(template.New("levels").Parse(`{{ range $name, $karma := . }}{{ $name }} has {{ $karma }} karma. {{ end }}`))
	templateBoard  = template.Must(template.New("board").Parse(
		`{{ range $entry := . }}* {{ $entry.Name }} has {{ $entry.Karma }} karma.
{{ end }}`))
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := run(ctx); err != nil {
		log.Fatalln(err)
	}
}

func run(ctx context.Context) error {
	conn, err := amqp.Dial(fmt.Sprintf("amqp://%s:%s@%s:%s", amqpUser, amqpPass, amqpHost, amqpPort))
	if err != nil {
		return err
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()

	err = ch.ExchangeDeclare(
		"popple_topic",
		"topic",
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return err
	}

	requestQueue, err := ch.QueueDeclare(
		"requests",
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return err
	}

	queue, err := ch.QueueDeclare(
		"",
		false,
		false,
		true,
		false,
		nil,
	)
	if err != nil {
		return err
	}

	err = ch.QueueBind(
		queue.Name,
		"checked.*",
		"popple_topic",
		false,
		nil,
	)
	if err != nil {
		return err
	}

	err = ch.QueueBind(
		queue.Name,
		"changed.*",
		"popple_topic",
		false,
		nil,
	)
	if err != nil {
		return err
	}

	events, err := ch.Consume(
		queue.Name,
		"",
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return err
	}

	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return err
	}

	err = session.Open()
	if err != nil {
		return err
	}
	defer session.Close()
	log.Println("connected to Discord")

	var wg sync.WaitGroup
	wg.Add(2)
	go publisher(ctx, &wg, ch, requestQueue, session)
	go consumer(ctx, &wg, events, session)

	wg.Wait()
	return nil
}

func publisher(ctx context.Context, wg *sync.WaitGroup, ch *amqp.Channel, qu amqp.Queue, session *discordgo.Session) {
	defer wg.Done()
	defer log.Println("publisher has stopped")

	mux := popple.NewMux("@" + session.State.User.Username)
	detachMessageCreate := session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if s.State.User.ID == m.Author.ID {
			return
		}

		// Direct message
		if len(m.GuildID) == 0 {
			return
		}

		message := strings.TrimSpace(m.ContentWithMentionsReplaced())
		action, body := mux.Route(message)

		switch action.(type) {
		case popple.AnnounceHandler:
			on, err := popple.ParseAnnounceArgs(body)
			if errors.Is(err, poperrs.ErrMissingArgument) || errors.Is(err, poperrs.ErrInvalidArgument) {
				err := s.MessageReactionAdd(m.ChannelID, m.ID, "❓")
				if err != nil {
					log.Println("failed to react to message:", err)
				}
				_, err = s.ChannelMessageSend(m.ChannelID, `Valid announce settings are: "on", "off", "yes", "no"`)
				if err != nil {
					log.Println("message send failed:", err)
				}
				return
			}

			var payload bytes.Buffer
			err = json.NewEncoder(&payload).Encode(
				event.Event{
					RequestChangeAnnounce: &event.RequestChangeAnnounce{
						ReactTo: event.ReactTo{
							ChannelID: m.ChannelID,
							MessageID: m.ID,
						},
						ServerID:   m.GuildID,
						NoAnnounce: !on,
					}})
			if err != nil {
				log.Println("failed to encode request.changeannounce:", err)
				return
			}

			err = ch.PublishWithContext(
				context.TODO(),
				"",
				qu.Name,
				false,
				false,
				amqp.Publishing{
					Body: payload.Bytes(),
				},
			)
			if err != nil {
				log.Println("failed to publish", payload, "err:", err)
			}
		case popple.BumpKarmaHandler:
			increments, _ := popple.ParseBumpKarmaArgs(body)

			var payload bytes.Buffer
			err := json.NewEncoder(&payload).Encode(event.Event{
				RequestBumpKarma: &event.RequestBumpKarma{
					ReplyTo: event.ReplyTo{
						ChannelID: m.ChannelID,
					},
					ServerID: m.GuildID,
					Who:      increments,
				}})
			if err != nil {
				log.Println("failed to encode request.bumpkarma:", err)
				return
			}

			err = ch.PublishWithContext(
				context.TODO(),
				"",
				qu.Name,
				false,
				false,
				amqp.Publishing{
					Body: payload.Bytes(),
				},
			)
			if err != nil {
				log.Println("failed to publish", payload, "err:", err)
			}
		case popple.KarmaHandler:
			who, err := popple.ParseKarmaArgs(body)
			if err != nil {
				err = s.MessageReactionAdd(m.ChannelID, m.ID, "❓")
				if err != nil {
					log.Println("message reaction add failed:", err)
					return
				}
			}

			var payload bytes.Buffer
			err = json.NewEncoder(&payload).Encode(event.Event{
				RequestCheckKarma: &event.RequestCheckKarma{
					ReplyTo: event.ReplyTo{
						ChannelID: m.ChannelID,
					},
					ServerID: m.GuildID,
					Who:      who,
				}})
			if err != nil {
				log.Println("failed to encode request.checkkarma:", err)
				return
			}

			err = ch.PublishWithContext(
				context.TODO(),
				"",
				qu.Name,
				false,
				false,
				amqp.Publishing{
					Body: payload.Bytes(),
				},
			)
			if err != nil {
				log.Println("failed to publish", payload, "err:", err)
			}
		case popple.LeaderboardHandler:
			limit, err := popple.ParseLeaderboardArgs(body)
			if errors.Is(err, poperrs.ErrInvalidArgument) {
				_, err := s.ChannelMessageSend(m.ChannelID, "The number of entries to list must be a positive non-zero integer")
				if err != nil {
					log.Println("message send failed:", err)
				}
				return
			}

			var payload bytes.Buffer
			err = json.NewEncoder(&payload).Encode(event.Event{
				RequestCheckLeaderboard: &event.RequestCheckLeaderboard{
					ReplyTo: event.ReplyTo{
						ChannelID: m.ChannelID,
					},
					ServerID: m.GuildID,
					Limit:    limit,
				}})
			if err != nil {
				log.Println("failed to encode request.checkleaderboard:", err)
				return
			}

			err = ch.PublishWithContext(
				context.TODO(),
				"",
				qu.Name,
				false,
				false,
				amqp.Publishing{
					Body: payload.Bytes(),
				},
			)
			if err != nil {
				log.Println("failed to publish", payload, "err:", err)
			}
		case popple.LoserboardHandler:
			limit, err := popple.ParseLoserboardArgs(body)
			if errors.Is(err, poperrs.ErrInvalidArgument) {
				_, err := s.ChannelMessageSend(m.ChannelID, "The number of entries to list must be a positive non-zero integer")
				if err != nil {
					log.Println("message send failed:", err)
				}
				return
			}

			var payload bytes.Buffer
			err = json.NewEncoder(&payload).Encode(event.Event{
				RequestCheckLoserboard: &event.RequestCheckLoserboard{
					ReplyTo: event.ReplyTo{
						ChannelID: m.ChannelID,
					},
					ServerID: m.GuildID,
					Limit:    limit,
				}})
			if err != nil {
				log.Println("failed to encode request.checkloserboard:", err)
				return
			}

			err = ch.PublishWithContext(
				context.TODO(),
				"",
				qu.Name,
				false,
				false,
				amqp.Publishing{
					Body: payload.Bytes(),
				},
			)
			if err != nil {
				log.Println("failed to publish", payload, "err:", err)
			}
		}
	})
	defer detachMessageCreate()
	log.Println("publisher has started")

	<-ctx.Done()
}

func consumer(ctx context.Context, wg *sync.WaitGroup, events <-chan amqp.Delivery, session *discordgo.Session) {
	defer wg.Done()
	defer log.Println("consumer has stopped")
	log.Println("consumer has started")

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				log.Println("consumer sees a closed events channel")
				return
			}

			var actual event.Event
			err := json.Unmarshal(evt.Body, &actual)
			if err != nil {
				log.Println("failed to deserialize event:", err)
				continue
			}

			eventJSON, _ := json.Marshal(actual)
			log.Println("got event", string(eventJSON))

			switch {
			case actual.CheckedKarma != nil:
				rsp := actual.CheckedKarma
				var r strings.Builder
				err := templateLevels.Execute(&r, rsp.Who)
				if err != nil {
					log.Println("failed to apply levels template:", err)
					continue
				}

				_, err = session.ChannelMessageSend(rsp.ReplyTo.ChannelID, r.String())
				if err != nil {
					log.Println("failed to send message:", err)
					continue
				}
			case actual.CheckedLeaderboard != nil:
				rsp := actual.CheckedLeaderboard
				var r strings.Builder
				err := templateBoard.Execute(&r, rsp.Board)
				if err != nil {
					log.Println("failed to apply board template:", err)
					continue
				}

				_, err = session.ChannelMessageSend(rsp.ReplyTo.ChannelID, r.String())
				if err != nil {
					log.Println("failed to send message:", err)
					continue
				}
			case actual.CheckedLoserboard != nil:
				rsp := actual.CheckedLoserboard
				var r strings.Builder
				err := templateBoard.Execute(&r, rsp.Board)
				if err != nil {
					log.Println("failed to apply board template:", err)
					continue
				}

				_, err = session.ChannelMessageSend(rsp.ReplyTo.ChannelID, r.String())
				if err != nil {
					log.Println("failed to send message:", err)
					continue
				}
			case actual.ChangedAnnounce != nil:
				rsp := actual.ChangedAnnounce
				err := session.MessageReactionAdd(rsp.ReactTo.ChannelID, rsp.ReactTo.MessageID, "✅")
				if err != nil {
					log.Println("failed to add reaction:", err)
					continue
				}
			case actual.ChangedKarma != nil:
				rsp := actual.ChangedKarma

				if !rsp.Announce {
					continue
				}

				var r strings.Builder
				err := templateLevels.Execute(&r, rsp.Who)
				if err != nil {
					log.Println("failed to apply levels template:", err)
					continue
				}

				_, err = session.ChannelMessageSend(rsp.ReplyTo.ChannelID, r.String())
				if err != nil {
					log.Println("failed to send message:", err)
					continue
				}
			default:
				log.Println("discarding unknown or unspecified event", evt)
			}
		}
	}
}
