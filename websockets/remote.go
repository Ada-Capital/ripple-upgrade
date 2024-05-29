package websockets

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Ada-Capital/ripple-upgrade/data"
	"github.com/golang/glog"
	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Time allowed to connect to server.
	dialTimeout = 5 * time.Second
)

type Remote struct {
	Incoming chan interface{}
	outgoing chan Syncer
	ws       *websocket.Conn

	endpoint string
}

// NewRemote returns a new remote session connected to the specified
// server endpoint URI. To close the connection, use Close().
func NewRemote(endpoint string) (*Remote, error) {
	glog.Infoln(endpoint)
	r := &Remote{
		outgoing: make(chan Syncer, 100),
		endpoint: endpoint,
	}

	go r.run()
	return r, nil
}

func NewRemoteHttp(endpoint string) (*Remote, error) {
	glog.Infoln(endpoint)
	r := &Remote{
		outgoing: make(chan Syncer, 100),
		endpoint: endpoint,
	}

	return r, nil
}

// Close shuts down the Remote session and blocks until all internal
// goroutines have been cleaned up.
// Any commands that are pending a response will return with an error.
func (r *Remote) Close() {
	close(r.outgoing)

	// Drain the Incoming channel and block until it is closed,
	// indicating that this Remote is fully cleaned up.
	for range r.Incoming {
	}
}

// run spawns the read/write pumps and then runs until Close() is called.
func (r *Remote) run() {

	var con net.Conn
	var ws *websocket.Conn

	for {
		u, err := url.Parse(r.endpoint)
		if err != nil {
			panic(err)
		}
		con, err = net.DialTimeout("tcp", u.Host, dialTimeout)
		if err != nil {
			glog.Errorln("Error dialing XRP. Retrying in 5 sec", err.Error())
			time.Sleep(5 * time.Second)
			continue
		}
		ws, _, err = websocket.NewClient(con, u, nil, 1024, 1024)
		if err != nil {
			glog.Errorln("Error dialing XRP. Retrying in 5 sec", err.Error())
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}
	r.ws = ws
	r.Incoming = make(chan interface{}, 1000)
	outbound := make(chan interface{})
	inbound := make(chan []byte)
	pending := make(map[uint64]Syncer)

	defer func() {
		close(outbound) // Shuts down the writePump
		close(r.Incoming)

		// Cancel all pending commands with an error
		for _, c := range pending {
			c.Fail("Connection Closed")
		}

		// Drain the inbound channel and block until it is closed,
		// indicating that the readPump has returned.
		for range inbound {
		}

		go r.run()

	}()

	// Spawn read/write goroutines
	go func() {
		defer r.ws.Close()
		r.writePump(outbound)
	}()
	go func() {
		defer close(inbound)
		r.readPump(inbound)
	}()

	// Main run loop
	var response Command
	for {
		select {
		case command, ok := <-r.outgoing:
			if !ok {
				return
			}
			outbound <- command
			id := reflect.ValueOf(command).Elem().FieldByName("Id").Uint()
			pending[id] = command

		case in, ok := <-inbound:
			if !ok {
				glog.Errorln("Connection closed by server")
				return
			}

			if err := json.Unmarshal(in, &response); err != nil {
				glog.Errorln(err.Error())
				continue
			}
			// Stream message
			factory, ok := streamMessageFactory[response.Type]
			if ok {
				cmd := factory()
				if err := json.Unmarshal(in, &cmd); err != nil {
					glog.Errorln(err.Error(), string(in))
					continue
				}
				r.Incoming <- cmd
				continue
			}

			// Command response message
			cmd, ok := pending[response.Id]
			if !ok {
				glog.Errorf("Unexpected message: %+v", response)
				continue
			}
			delete(pending, response.Id)
			if err := json.Unmarshal(in, &cmd); err != nil {
				msg := fmt.Sprintf("%s ---- %s", err.Error(), string(in))
				glog.Errorln(msg)
				cmd.Fail(msg)
				continue
			}
			cmd.Done()
		}
	}
}

// Synchronously get a single transaction
func (r *Remote) Tx(hash data.Hash256) (*TxResult, error) {
	cmd := &TxCommand{
		Command:     newCommand("tx"),
		Transaction: hash,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

func (r *Remote) TxHttp(hash data.Hash256) (*TxResult, error) {
	cmd := &TxCommand{
		Command:     newCommand("tx"),
		Transaction: hash,
	}

	respCmd, err := r.httpReqFromWs(cmd)
	if err != nil {
		return nil, err
	}

	cmd = respCmd.(*TxCommand)

	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

func (r *Remote) accountTx(account data.Account, c chan *data.TransactionWithMetaData, pageSize int, minLedger, maxLedger int64) {
	defer close(c)
	cmd := newAccountTxCommand(account, pageSize, nil, minLedger, maxLedger)
	for ; ; cmd = newAccountTxCommand(account, pageSize, cmd.Result.Marker, minLedger, maxLedger) {
		r.outgoing <- cmd
		<-cmd.Ready
		if cmd.CommandError != nil {
			glog.Errorln(cmd.Error())
			c <- nil
			return
		}
		for _, tx := range cmd.Result.Transactions {
			c <- tx
		}
		if cmd.Result.Marker == nil {
			return
		}
	}
}

// Retrieve all transactions for an account via
// https://ripple.com/build/rippled-apis/#account-tx. Will call
// `account_tx` multiple times, if a marker is returned.  Transactions
// are returned asynchonously to the channel returned by this
// function.
//
// Use minLedger -1 for the earliest ledger available.
// Use maxLedger -1 for the most recent validated ledger.
func (r *Remote) AccountTx(account data.Account, pageSize int, minLedger, maxLedger int64) chan *data.TransactionWithMetaData {
	c := make(chan *data.TransactionWithMetaData)
	go r.accountTx(account, c, pageSize, minLedger, maxLedger)
	return c
}

func (r *Remote) AccountTxHttp(account data.Account, pageSize int, minLedger, maxLedger int64) ([]*data.TransactionWithMetaData, error) {
	var resp []*data.TransactionWithMetaData
	cmd := newAccountTxCommand(account, pageSize, nil, minLedger, maxLedger)
	for ; ; cmd = newAccountTxCommand(account, pageSize, cmd.Result.Marker, minLedger, maxLedger) {

		respCmd, err := r.httpReqFromWs(cmd)
		if err != nil {
			return nil, err
		}

		cmd = respCmd.(*AccountTxCommand)

		if cmd.CommandError != nil {
			return nil, errors.New(cmd.Error())
		}
		for _, tx := range cmd.Result.Transactions {
			resp = append(resp, tx)
		}
		if cmd.Result.Marker == nil {
			return resp, nil
		}
	}
}

// Synchronously submit a single transaction
func (r *Remote) Submit(tx data.Transaction) (*SubmitResult, error) {
	_, raw, err := data.Raw(tx)
	if err != nil {
		return nil, err
	}
	cmd := &SubmitCommand{
		Command: newCommand("submit"),
		TxBlob:  fmt.Sprintf("%X", raw),
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

func (r *Remote) SubmitHttp(tx data.Transaction) (*SubmitResult, error) {
	_, raw, err := data.Raw(tx)
	if err != nil {
		return nil, err
	}
	cmd := &SubmitCommand{
		Command: newCommand("submit"),
		TxBlob:  fmt.Sprintf("%X", raw),
	}

	respCmd, err := r.httpReqFromWs(cmd)
	if err != nil {
		return nil, err
	}

	cmd = respCmd.(*SubmitCommand)

	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// Synchronously submit multiple transactions
func (r *Remote) SubmitBatch(txs []data.Transaction) ([]*SubmitResult, error) {
	commands := make([]*SubmitCommand, len(txs))
	results := make([]*SubmitResult, len(txs))
	for i := range txs {
		_, raw, err := data.Raw(txs[i])
		if err != nil {
			return nil, err
		}
		cmd := &SubmitCommand{
			Command: newCommand("submit"),
			TxBlob:  fmt.Sprintf("%X", raw),
		}
		r.outgoing <- cmd
		commands[i] = cmd
	}
	for i := range commands {
		<-commands[i].Ready
		results[i] = commands[i].Result
	}
	return results, nil
}

// Synchronously gets ledger entries
func (r *Remote) LedgerData(ledger interface{}, marker *data.Hash256) (*LedgerDataResult, error) {
	cmd := &LedgerDataCommand{
		Command: newCommand("ledger_data"),
		Ledger:  ledger,
		Marker:  marker,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

func (r *Remote) streamLedgerData(ledger interface{}, start, end string, c chan data.LedgerEntrySlice, wg *sync.WaitGroup) {
	defer wg.Done()
	first, err := data.NewHash256(start)
	if err != nil {
		glog.Errorln(err.Error())
	}
	cmd := newBinaryLedgerDataCommand(ledger, first)
	var br bytes.Reader
	for ; ; cmd = newBinaryLedgerDataCommand(ledger, cmd.Result.Marker) {
		r.outgoing <- cmd
		<-cmd.Ready
		if cmd.CommandError != nil {
			glog.Errorln(cmd.Error())
			return
		}
		les := make(data.LedgerEntrySlice, 0, len(cmd.Result.State))
		var done bool
		for _, state := range cmd.Result.State {
			if done = state.Index > end; done {
				break
			}
			b, err := hex.DecodeString(state.Data + state.Index)
			if err != nil {
				glog.Errorln(err.Error())
				return
			}
			br.Reset(b)
			le, err := data.ReadLedgerEntry(&br, data.Hash256{})
			if err != nil {
				glog.Errorln(err.Error())
				glog.Errorln(state.Data)
				glog.Errorln(state.Index)
				continue
			}
			les = append(les, le)
		}
		c <- les
		if cmd.Result.Marker == nil || done {
			return
		}
	}
}

// Asynchronously retrieve all data for a ledger using the binary form
func (r *Remote) StreamLedgerData(ledger interface{}) chan data.LedgerEntrySlice {
	c := make(chan data.LedgerEntrySlice, 100)
	wg := &sync.WaitGroup{}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		start := fmt.Sprintf("%X%s", i, strings.Repeat("0", 63))
		end := fmt.Sprintf("%X%s", i, strings.Repeat("F", 63))
		go r.streamLedgerData(ledger, start, end, c, wg)
	}
	go func() {
		wg.Wait()
		close(c)
	}()
	return c
}

// Synchronously gets a single ledger
func (r *Remote) Ledger(ledger interface{}, transactions bool) (*LedgerResult, error) {
	cmd := &LedgerCommand{
		Command:      newCommand("ledger"),
		LedgerIndex:  ledger,
		Transactions: transactions,
		Expand:       true,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	cmd.Result.Ledger.Transactions.Sort()
	return cmd.Result, nil
}

func (r *Remote) LedgerHttp(ledger interface{}, transactions bool) (*LedgerResult, error) {
	cmd := &LedgerCommand{
		Command:      newCommand("ledger"),
		LedgerIndex:  ledger,
		Transactions: transactions,
		Expand:       true,
	}

	respCmd, err := r.httpReqFromWs(cmd)
	if err != nil {
		return nil, err
	}

	cmd = respCmd.(*LedgerCommand)

	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	cmd.Result.Ledger.Transactions.Sort()
	return cmd.Result, nil
}

func (r *Remote) LedgerHeader(ledger interface{}) (*LedgerHeaderResult, error) {
	cmd := &LedgerHeaderCommand{
		Command: newCommand("ledger_header"),
		Ledger:  ledger,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// Synchronously requests paths
func (r *Remote) RipplePathFind(src, dest data.Account, amount data.Amount, srcCurr *[]data.Currency) (*RipplePathFindResult, error) {
	cmd := &RipplePathFindCommand{
		Command:       newCommand("ripple_path_find"),
		SrcAccount:    src,
		SrcCurrencies: srcCurr,
		DestAccount:   dest,
		DestAmount:    amount,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// Synchronously requests account info
func (r *Remote) AccountInfo(a data.Account) (*AccountInfoResult, error) {
	cmd := &AccountInfoCommand{
		Command: newCommand("account_info"),
		Account: a,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

func (r *Remote) AccountInfoHttp(a data.Account) (*AccountInfoResult, error) {
	cmd := &AccountInfoCommand{
		Command: newCommand("account_info"),
		Account: a,
	}

	respCmd, err := r.httpReqFromWs(cmd)
	if err != nil {
		return nil, err
	}

	cmd = respCmd.(*AccountInfoCommand)

	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// Synchronously requests account objects
func (r *Remote) AccountObjectsTickets(a data.Account) (*AccountObjectsTicketsResult, error) {
	cmd := &AccountObjectsTicketsCommand{
		Command: newCommand("account_objects"),
		Account: a,
		Type:    "ticket",
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

func (r *Remote) AccountObjectsTicketsHttp(a data.Account) (*AccountObjectsTicketsResult, error) {
	cmd := &AccountObjectsTicketsCommand{
		Command: newCommand("account_objects"),
		Account: a,
		Type:    "ticket",
	}

	respCmd, err := r.httpReqFromWs(cmd)
	if err != nil {
		return nil, err
	}

	cmd = respCmd.(*AccountObjectsTicketsCommand)

	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// Synchronously requests account line info
func (r *Remote) AccountLines(account data.Account, ledgerIndex interface{}) (*AccountLinesResult, error) {
	var (
		lines  data.AccountLineSlice
		marker *data.Hash256
	)
	for {
		cmd := &AccountLinesCommand{
			Command:     newCommand("account_lines"),
			Account:     account,
			Limit:       400,
			Marker:      marker,
			LedgerIndex: ledgerIndex,
		}
		r.outgoing <- cmd
		<-cmd.Ready
		switch {
		case cmd.CommandError != nil:
			return nil, cmd.CommandError
		case cmd.Result.Marker != nil:
			lines = append(lines, cmd.Result.Lines...)
			marker = cmd.Result.Marker
			if cmd.Result.LedgerSequence != nil {
				ledgerIndex = *cmd.Result.LedgerSequence
			}
		default:
			cmd.Result.Lines = append(lines, cmd.Result.Lines...)
			cmd.Result.Lines.SortByCurrencyAmount()
			return cmd.Result, nil
		}
	}
}

// Synchronously requests account line info
func (r *Remote) AccountLinesHttp(account data.Account, ledgerIndex interface{}) (*AccountLinesResult, error) {
	var (
		lines  data.AccountLineSlice
		marker *data.Hash256
	)
	for {
		cmd := &AccountLinesCommand{
			Command:     newCommand("account_lines"),
			Account:     account,
			Limit:       400,
			Marker:      marker,
			LedgerIndex: ledgerIndex,
		}

		respCmd, err := r.httpReqFromWs(cmd)
		if err != nil {
			return nil, err
		}

		cmd = respCmd.(*AccountLinesCommand)

		if cmd.Result.Marker != nil {
			lines = append(lines, cmd.Result.Lines...)
			marker = cmd.Result.Marker
			if cmd.Result.LedgerSequence != nil {
				ledgerIndex = *cmd.Result.LedgerSequence
			}
		} else {
			cmd.Result.Lines = append(lines, cmd.Result.Lines...)
			cmd.Result.Lines.SortByCurrencyAmount()
			return cmd.Result, nil
		}
	}
}

func (r *Remote) httpReqFromWs(cmd interface{}) (interface{}, error) {

	bodyData, _ := json.Marshal(cmd)

	newJson, err := transformJSON(string(bodyData))
	if err != nil {
		return nil, err
	}

	res, err := http.Post(r.endpoint, "application/json", bytes.NewReader([]byte(newJson)))
	if err != nil {
		return nil, err
	}

	respData, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		return nil, errors.New(string(respData))
	}

	err = json.Unmarshal(respData, &cmd)
	if err != nil {
		return nil, err
	}

	_ = res.Body.Close()

	return cmd, nil

}

func transformJSON(originalJSON string) (string, error) {
	// Unmarshal the original JSON into a map
	var originalMap map[string]interface{}
	if err := json.Unmarshal([]byte(originalJSON), &originalMap); err != nil {
		return "", err
	}

	// Create a new map for the transformed JSON
	transformedMap := make(map[string]interface{})

	// Move fields to the new map, transforming as needed
	if command, ok := originalMap["command"].(string); ok {
		transformedMap["method"] = command
		delete(originalMap, "command")
	}

	delete(originalMap, "id") // Remove the "id" field

	// Create the params list with a single map containing the remaining fields
	transformedMap["params"] = []map[string]interface{}{originalMap}

	// Marshal the transformed map back into JSON
	transformedJSON, err := json.Marshal(transformedMap)
	if err != nil {
		return "", err
	}

	return string(transformedJSON), nil
}

// Synchronously requests account offers
func (r *Remote) AccountOffers(account data.Account, ledgerIndex interface{}) (*AccountOffersResult, error) {
	var (
		offers data.AccountOfferSlice
		marker *data.Hash256
	)
	for {
		cmd := &AccountOffersCommand{
			Command:     newCommand("account_offers"),
			Account:     account,
			Limit:       400,
			Marker:      marker,
			LedgerIndex: ledgerIndex,
		}
		r.outgoing <- cmd
		<-cmd.Ready
		switch {
		case cmd.CommandError != nil:
			return nil, cmd.CommandError
		case cmd.Result.Marker != nil:
			offers = append(offers, cmd.Result.Offers...)
			marker = cmd.Result.Marker
			if cmd.Result.LedgerSequence != nil {
				ledgerIndex = *cmd.Result.LedgerSequence
			}
		default:
			cmd.Result.Offers = append(offers, cmd.Result.Offers...)
			sort.Sort(cmd.Result.Offers)
			return cmd.Result, nil
		}
	}
}

func (r *Remote) BookOffers(taker data.Account, ledgerIndex interface{}, pays, gets data.Asset) (*BookOffersResult, error) {
	cmd := &BookOffersCommand{
		Command:     newCommand("book_offers"),
		LedgerIndex: ledgerIndex,
		Taker:       taker,
		TakerPays:   pays,
		TakerGets:   gets,
		Limit:       5000, // Marker not implemented....
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// Synchronously subscribe to streams and receive a confirmation message
// Streams are recived asynchronously over the Incoming channel
func (r *Remote) Subscribe(ledger, transactions, transactionsProposed, server bool, accounts []data.Account) (*SubscribeResult, error) {
	streams := []string{}
	if ledger {
		streams = append(streams, "ledger")
	}
	if transactions {
		streams = append(streams, "transactions")
	}
	if transactionsProposed {
		streams = append(streams, "transactions_proposed")
	}
	if server {
		streams = append(streams, "server")
	}
	cmd := &SubscribeCommand{
		Command:  newCommand("subscribe"),
		Streams:  streams,
		Accounts: accounts,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}

	if ledger && cmd.Result.LedgerStreamMsg == nil {
		return nil, fmt.Errorf("Missing ledger subscribe response")
	}
	if server && cmd.Result.ServerStreamMsg == nil {
		return nil, fmt.Errorf("Missing server subscribe response")
	}
	return cmd.Result, nil
}

type OrderBookSubscription struct {
	TakerGets data.Asset `json:"taker_gets"`
	TakerPays data.Asset `json:"taker_pays"`
	Snapshot  bool       `json:"snapshot"`
	Both      bool       `json:"both"`
}

func (r *Remote) SubscribeOrderBooks(books []OrderBookSubscription) (*SubscribeResult, error) {
	cmd := &SubscribeCommand{
		Command: newCommand("subscribe"),
		Streams: []string{"ledger", "server"},
		Books:   books,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

func (r *Remote) Fee() (*FeeResult, error) {
	cmd := &FeeCommand{
		Command: newCommand("fee"),
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

func (r *Remote) FeeHttp() (*FeeResult, error) {
	cmd := &FeeCommand{
		Command: newCommand("fee"),
	}

	respCmd, err := r.httpReqFromWs(cmd)
	if err != nil {
		return nil, err
	}

	cmd = respCmd.(*FeeCommand)

	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// readPump reads from the websocket and sends to inbound channel.
// Expects to receive PONGs at specified interval, or logs an error and returns.
func (r *Remote) readPump(inbound chan<- []byte) {
	r.ws.SetReadDeadline(time.Now().Add(pongWait))
	r.ws.SetPongHandler(func(string) error { r.ws.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := r.ws.ReadMessage()
		if err != nil {
			glog.Errorln(err)
			return
		}
		if glog.V(2) {
			glog.Infoln(dump(message))
		}
		r.ws.SetReadDeadline(time.Now().Add(pongWait))
		inbound <- message
	}
}

// Consumes from the outbound channel and sends them over the websocket.
// Also sends PING messages at the specified interval.
// Returns when outbound channel is closed, or an error is encountered.
func (r *Remote) writePump(outbound <-chan interface{}) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {

		// An outbound message is available to send
		case message, ok := <-outbound:
			if !ok {
				r.ws.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			b, err := json.Marshal(message)
			if err != nil {
				// Outbound message cannot be JSON serialized (log it and continue)
				glog.Errorln(err)
				continue
			}
			if glog.V(2) {
				glog.Infoln(dump(b))
			}
			if err := r.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				glog.Errorln(err)
				return
			}

		// Time to send a ping
		case <-ticker.C:
			if err := r.ws.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				glog.Errorln(err)
				return
			}
		}
	}
}

func dump(b []byte) string {
	var v map[string]interface{}
	json.Unmarshal(b, &v)
	out, _ := json.MarshalIndent(v, "", "  ")
	return string(out)
}
