package telegram

import (
	"database/sql"
	"log"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters/message"
	"github.com/google/uuid"
	"github.com/jellydator/ttlcache/v3"
	"github.com/oybek/choguuket/database"
	"github.com/oybek/choguuket/model"
	"github.com/oybek/choguuket/service"
	"github.com/samber/lo"

	"github.com/sashabaranov/go-openai"
)

type LongPoll struct {
	bot          *gotgbot.Bot
	db           *sql.DB
	openaiClient *openai.Client
	readers      map[string]service.ExcelReader
	requestCache *ttlcache.Cache[uuid.UUID, []lo.Tuple2[model.Apteka, []string]]
}

func NewLongPoll(
	bot *gotgbot.Bot,
	db *sql.DB,
	openaiClient *openai.Client,
	readers map[string]service.ExcelReader,
	requestCache *ttlcache.Cache[uuid.UUID, []lo.Tuple2[model.Apteka, []string]],
) *LongPoll {
	return &LongPoll{
		bot:          bot,
		db:           db,
		openaiClient: openaiClient,
		readers:      readers,
		requestCache: requestCache,
	}
}

const createAptekaWebAppUrl = "https://wolfrepos.github.io/apteka/create/index.html"

func (lp *LongPoll) Run() {
	dispatcher := ext.NewDispatcher(&ext.DispatcherOpts{
		Error: func(b *gotgbot.Bot, ctx *ext.Context, err error) ext.DispatcherAction {
			log.Println("an error occurred while handling update:", err.Error())
			return ext.DispatcherActionNoop
		},
		MaxRoutines: ext.DefaultMaxRoutines,
	})
	updater := ext.NewUpdater(dispatcher, nil)

	dispatcher.AddHandler(handlers.NewMessage(
		func(msg *gotgbot.Message) bool { return msg.WebAppData != nil },
		lp.handleWebAppData,
	))
	dispatcher.AddHandler(handlers.NewMessage(message.Document, lp.handleDocument))
	dispatcher.AddHandler(handlers.NewMessage(message.Command, lp.handleCommand))
	dispatcher.AddHandler(handlers.NewMessage(message.Voice, lp.handleVoice))
	dispatcher.AddHandler(handlers.NewMessage(message.Text, lp.handleText))

	// Start receiving updates.
	err := updater.StartPolling(lp.bot, &ext.PollingOpts{
		DropPendingUpdates: true,
		GetUpdatesOpts: &gotgbot.GetUpdatesOpts{
			Timeout: 9,
			RequestOpts: &gotgbot.RequestOpts{
				Timeout: time.Second * 10,
			},
		},
	})
	if err != nil {
		panic("failed to start polling: " + err.Error())
	}

	lp.SetupCommands()

	log.Printf("%s has been started...\n", lp.bot.User.Username)

	// Idle, to keep updates coming in, and avoid bot stopping.
	updater.Idle()
}

func (lp *LongPoll) handleText(b *gotgbot.Bot, ctx *ext.Context) error {
	chat := ctx.EffectiveMessage.Chat
	return lp.sendText(chat.Id, TextDefault)
}

func (lp *LongPoll) sendText(chatId int64, text string) error {
	_, err := lp.bot.SendMessage(chatId, text, &gotgbot.SendMessageOpts{})
	return err
}

func (lp *LongPoll) handleWebAppData(b *gotgbot.Bot, ctx *ext.Context) error {
	webAppData := ctx.EffectiveMessage.WebAppData
	if webAppData == nil {
		return nil
	}

	chat := &ctx.EffectiveMessage.Chat
	lp.bot.DeleteMessage(chat.Id, ctx.EffectiveMessage.MessageId, &gotgbot.DeleteMessageOpts{})
	json := webAppData.Data
	log.Printf("[ChatId=%d] Got json from WebApp: %s", chat.Id, json)

	if apteka, err := model.ParseAndValidate[model.Apteka](json); err == nil {
		return lp.handleWebAppApteka(chat, apteka)
	}

	return lp.sendText(chat.Id, "Что-то пошло не так - попробуйте еще раз")
}

func (lp *LongPoll) handleWebAppApteka(chat *gotgbot.Chat, apteka *model.Apteka) error {
	_, err := database.Transact(lp.db, func(tx database.TransactionOps) (bool, error) {
		aptekaId, err := database.AptekaInsert(tx, apteka)
		if err != nil {
			return false, err
		}

		user := model.User{ChatId: chat.Id, AptekaId: int64(aptekaId), Reader: "test"}
		err = database.UserInsert(tx, &user)
		if err != nil {
			return false, err
		}

		return true, nil
	})

	if err != nil {
		return err
	}

	return lp.sendText(chat.Id, "Аптека успешно создана ✅")
}
