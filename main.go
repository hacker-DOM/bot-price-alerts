package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "github.com/mattn/go-sqlite3"
)

const (
	coingeckoURL = "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin,ethereum&vs_currencies=usd"
)

var (
	bot         *tgbotapi.BotAPI
	db          *sql.DB
	httpClient  = &http.Client{Timeout: 10 * time.Second}
	assetOrder  = []string{"bitcoin", "ethereum"}
	assetLabels = map[string]string{"bitcoin": "BTC", "ethereum": "ETH"}
	botUsername string

	// Per-chat state
	prevPrices = map[int64]map[string]float64{}
	chatSubs   = map[int64]struct{}{}
	stateMu    sync.RWMutex
)

type priceResponse map[string]struct {
	USD float64 `json:"usd"`
}

func main() {
	loadEnvFile(".env")

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}

	var err error
	bot, err = tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Panic(err)
	}
	bot.Debug = false
	botUsername = bot.Self.UserName

	dbPath := os.Getenv("TELEGRAM_DB_PATH")
	if dbPath == "" {
		dbPath = "bot.db"
	}
	db, err = initDB(dbPath)
	if err != nil {
		log.Fatalf("failed to init db: %v", err)
	}
	if err := loadState(); err != nil {
		log.Fatalf("failed to load state: %v", err)
	}
	// Ensure hard-coded subscription is not present
	removeSubscriber(-5021234433)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	go receiveUpdates(ctx, updates)
	go runPriceLoop(ctx)

	log.Println("Price alerts bot running. Press Enter to stop.")
	bufio.NewReader(os.Stdin).ReadBytes('\n')
	cancel()
	// give goroutine a moment to exit cleanly
	time.Sleep(200 * time.Millisecond)
}

func receiveUpdates(ctx context.Context, updates tgbotapi.UpdatesChannel) {
	for {
		select {
		case <-ctx.Done():
			return
		case update := <-updates:
			handleUpdate(update)
		}
	}
}

func handleUpdate(update tgbotapi.Update) {
	if update.Message != nil {
		handleMessage(update.Message)
	}
}

func handleMessage(message *tgbotapi.Message) {
	if message == nil || message.From == nil {
		return
	}

	text := message.Text
	if strings.HasPrefix(text, "/") {
		if err := handleCommand(message.Chat.ID, text); err != nil {
			log.Printf("command error for chat %d: %v", message.Chat.ID, err)
		}
		return
	}

	if shouldReplyWithPrices(message) {
		if err := sendCurrentPrices(message.Chat.ID); err != nil {
			log.Printf("price reply failed for chat %d: %v", message.Chat.ID, err)
		}
	}
}

func handleCommand(chatID int64, command string) error {
	switch command {
	case "/start", "/alerts_on":
		addSubscriber(chatID)
		_, err := bot.Send(tgbotapi.NewMessage(chatID, "Subscribed: hourly BTC/ETH alerts enabled."))
		return err
	case "/stop", "/alerts_off":
		removeSubscriber(chatID)
		_, err := bot.Send(tgbotapi.NewMessage(chatID, "Unsubscribed: alerts disabled."))
		return err
	case "/status":
		stateMu.RLock()
		_, subscribed := chatSubs[chatID]
		stateMu.RUnlock()
		status := "not subscribed"
		if subscribed {
			status = "subscribed"
		}
		_, err := bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("Status: %s to hourly alerts.", status)))
		return err
	case "/price", "/prices", "/ping":
		return sendCurrentPrices(chatID)
	default:
		_, err := bot.Send(tgbotapi.NewMessage(chatID, "Commands: /start or /alerts_on, /stop or /alerts_off, /status, /price"))
		return err
	}
}

func runPriceLoop(ctx context.Context) {
	for {
		now := time.Now()
		next := now.Truncate(time.Hour).Add(time.Hour)
		wait := time.Until(next)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if err := fetchAndAlert(ctx); err != nil {
			log.Printf("hourly fetch failed: %v", err)
		}
	}
}

func fetchAndAlert(ctx context.Context) error {
	prices, err := fetchPrices(ctx)
	if err != nil {
		return err
	}

	chatIDs := subscriberIDs()
	for _, chatID := range chatIDs {
		changes := computeChanges(chatID, prices)
		if len(changes) == 0 {
			continue
		}
		text := "Hourly change vs last price:\n" + strings.Join(changes, "\n")
		msg := tgbotapi.NewMessage(chatID, text)
		if _, err := bot.Send(msg); err != nil {
			log.Printf("send error to chat %d: %v", chatID, err)
		}
	}

	return nil
}

func fetchPrices(ctx context.Context) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coingeckoURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coingecko returned status %s", resp.Status)
	}

	var decoded priceResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}

	prices := map[string]float64{}
	for asset := range assetLabels {
		if entry, ok := decoded[asset]; ok {
			prices[asset] = entry.USD
		}
	}
	return prices, nil
}

func formatSign(v float64) string {
	if v >= 0 {
		return "+"
	}
	return ""
}

func subscriberIDs() []int64 {
	stateMu.RLock()
	defer stateMu.RUnlock()
	ids := make([]int64, 0, len(chatSubs))
	for id := range chatSubs {
		ids = append(ids, id)
	}
	return ids
}

func addSubscriber(chatID int64) {
	stateMu.Lock()
	defer stateMu.Unlock()
	chatSubs[chatID] = struct{}{}
	if _, ok := prevPrices[chatID]; !ok {
		prevPrices[chatID] = map[string]float64{}
	}
	if err := upsertSubscription(chatID); err != nil {
		log.Printf("failed to persist subscription %d: %v", chatID, err)
	}
}

func removeSubscriber(chatID int64) {
	stateMu.Lock()
	defer stateMu.Unlock()
	delete(chatSubs, chatID)
	delete(prevPrices, chatID)
	if err := deleteSubscription(chatID); err != nil {
		log.Printf("failed to remove subscription %d: %v", chatID, err)
	}
	if err := deletePrices(chatID); err != nil {
		log.Printf("failed to delete prices for %d: %v", chatID, err)
	}
}

func computeChanges(chatID int64, prices map[string]float64) []string {
	stateMu.Lock()
	defer stateMu.Unlock()

	prev, ok := prevPrices[chatID]
	if !ok {
		prev = map[string]float64{}
		prevPrices[chatID] = prev
	}

	var changes []string
	for _, asset := range assetOrder {
		current, exists := prices[asset]
		if !exists {
			continue
		}
		prevVal, hasPrev := prev[asset]
		if !hasPrev || prevVal == 0 {
			prev[asset] = current
			if err := upsertPrice(chatID, asset, current); err != nil {
				log.Printf("failed to persist initial price for chat %d asset %s: %v", chatID, asset, err)
			}
			continue
		}
		diff := ((current - prevVal) / prevVal) * 100
		label := assetLabels[asset]
		changes = append(changes, fmt.Sprintf("%s: %s%.2f%% ($%.2f)", label, formatSign(diff), diff, current))
		prev[asset] = current
		if err := upsertPrice(chatID, asset, current); err != nil {
			log.Printf("failed to persist price for chat %d asset %s: %v", chatID, asset, err)
		}
	}
	return changes
}

func initDB(path string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	schema := `
CREATE TABLE IF NOT EXISTS subscriptions (
    chat_id INTEGER PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS prices (
    chat_id INTEGER,
    asset TEXT,
    price REAL,
    PRIMARY KEY(chat_id, asset)
);
`
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func loadState() error {
	stateMu.Lock()
	defer stateMu.Unlock()

	chatSubs = map[int64]struct{}{}
	prevPrices = map[int64]map[string]float64{}

	rows, err := db.Query("SELECT chat_id FROM subscriptions")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		chatSubs[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	priceRows, err := db.Query("SELECT chat_id, asset, price FROM prices")
	if err != nil {
		return err
	}
	defer priceRows.Close()
	for priceRows.Next() {
		var chatID int64
		var asset string
		var price float64
		if err := priceRows.Scan(&chatID, &asset, &price); err != nil {
			return err
		}
		if _, ok := prevPrices[chatID]; !ok {
			prevPrices[chatID] = map[string]float64{}
		}
		prevPrices[chatID][asset] = price
	}
	return priceRows.Err()
}

func upsertSubscription(chatID int64) error {
	_, err := db.Exec("INSERT OR IGNORE INTO subscriptions(chat_id) VALUES(?)", chatID)
	return err
}

func deleteSubscription(chatID int64) error {
	_, err := db.Exec("DELETE FROM subscriptions WHERE chat_id = ?", chatID)
	return err
}

func upsertPrice(chatID int64, asset string, price float64) error {
	_, err := db.Exec("INSERT INTO prices(chat_id, asset, price) VALUES(?, ?, ?) ON CONFLICT(chat_id, asset) DO UPDATE SET price=excluded.price", chatID, asset, price)
	return err
}

func deletePrices(chatID int64) error {
	_, err := db.Exec("DELETE FROM prices WHERE chat_id = ?", chatID)
	return err
}

func sendCurrentPrices(chatID int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	prices, err := fetchPrices(ctx)
	if err != nil {
		return err
	}

	var lines []string
	for _, asset := range assetOrder {
		price, ok := prices[asset]
		if !ok {
			continue
		}
		label := assetLabels[asset]
		lines = append(lines, fmt.Sprintf("%s: $%.2f", label, price))
	}
	if len(lines) == 0 {
		return fmt.Errorf("no prices available")
	}
	msg := tgbotapi.NewMessage(chatID, "Current prices:\n"+strings.Join(lines, "\n"))
	_, err = bot.Send(msg)
	return err
}

func shouldReplyWithPrices(msg *tgbotapi.Message) bool {
	if msg.Chat != nil && msg.Chat.IsPrivate() {
		return true
	}
	if botUsername == "" {
		return false
	}
	mention := "@" + botUsername
	if strings.Contains(msg.Text, mention) {
		return true
	}
	return false
}

func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if key == "" {
			continue
		}
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("env file scan error: %v", err)
	}
}
