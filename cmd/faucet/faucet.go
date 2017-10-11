// Copyright 2017 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

// faucet is a Ether faucet backed by a light client.
package main

//go:generate go-bindata -nometadata -o website.go faucet.html

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bitherhq/go-bither/accounts"
	"github.com/bitherhq/go-bither/accounts/keystore"
	"github.com/bitherhq/go-bither/common"
	"github.com/bitherhq/go-bither/core"
	"github.com/bitherhq/go-bither/core/types"
	"github.com/bitherhq/go-bither/eth"
	"github.com/bitherhq/go-bither/eth/downloader"
	"github.com/bitherhq/go-bither/ethclient"
	"github.com/bitherhq/go-bither/ethstats"
	"github.com/bitherhq/go-bither/les"
	"github.com/bitherhq/go-bither/log"
	"github.com/bitherhq/go-bither/node"
	"github.com/bitherhq/go-bither/p2p"
	"github.com/bitherhq/go-bither/p2p/discover"
	"github.com/bitherhq/go-bither/p2p/discv5"
	"github.com/bitherhq/go-bither/p2p/nat"
	"github.com/bitherhq/go-bither/params"
	"golang.org/x/net/websocket"
)

var (
	genesisFlag = flag.String("genesis", "", "Genesis json file to seed the chain with")
	apiPortFlag = flag.Int("apiport", 8080, "Listener port for the HTTP API connection")
	ethPortFlag = flag.Int("ethport", 41207, "Listener port for the devp2p connection")
	bootFlag    = flag.String("bootnodes", "", "Comma separated bootnode enode URLs to seed with")
	netFlag     = flag.Uint64("network", 0, "Network ID to use for the Ethereum protocol")
	statsFlag   = flag.String("ethstats", "", "Ethstats network monitoring auth string")

	netnameFlag = flag.String("faucet.name", "", "Network name to assign to the faucet")
	payoutFlag  = flag.Int("faucet.amount", 1, "Number of Ethers to pay out per user request")
	minutesFlag = flag.Int("faucet.minutes", 1440, "Number of minutes to wait between funding rounds")
	tiersFlag   = flag.Int("faucet.tiers", 3, "Number of funding tiers to enable (x3 time, x2.5 funds)")

	accJSONFlag = flag.String("account.json", "", "Key json file to fund user requests with")
	accPassFlag = flag.String("account.pass", "", "Decryption password to access faucet funds")

	githubUser  = flag.String("github.user", "", "GitHub user to authenticate with for Gist access")
	githubToken = flag.String("github.token", "", "GitHub personal token to access Gists with")

	captchaToken  = flag.String("captcha.token", "", "Recaptcha site key to authenticate client side")
	captchaSecret = flag.String("captcha.secret", "", "Recaptcha secret key to authenticate server side")

	logFlag = flag.Int("loglevel", 3, "Log level to use for Ethereum and the faucet")
)

var (
	ether = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
)

func main() {
	// Parse the flags and set up the logger to print everything requested
	flag.Parse()
	log.Root().SetHandler(log.LvlFilterHandler(log.Lvl(*logFlag), log.StreamHandler(os.Stderr, log.TerminalFormat(true))))

	// Construct the payout tiers
	amounts := make([]string, *tiersFlag)
	periods := make([]string, *tiersFlag)
	for i := 0; i < *tiersFlag; i++ {
		// Calculate the amount for the next tier and format it
		amount := float64(*payoutFlag) * math.Pow(2.5, float64(i))
		amounts[i] = fmt.Sprintf("%s Ethers", strconv.FormatFloat(amount, 'f', -1, 64))
		if amount == 1 {
			amounts[i] = strings.TrimSuffix(amounts[i], "s")
		}
		// Calculate the period for the next tier and format it
		period := *minutesFlag * int(math.Pow(3, float64(i)))
		periods[i] = fmt.Sprintf("%d mins", period)
		if period%60 == 0 {
			period /= 60
			periods[i] = fmt.Sprintf("%d hours", period)

			if period%24 == 0 {
				period /= 24
				periods[i] = fmt.Sprintf("%d days", period)
			}
		}
		if period == 1 {
			periods[i] = strings.TrimSuffix(periods[i], "s")
		}
	}
	// Load up and render the faucet website
	tmpl, err := Asset("faucet.html")
	if err != nil {
		log.Crit("Failed to load the faucet template", "err", err)
	}
	website := new(bytes.Buffer)
	err = template.Must(template.New("").Parse(string(tmpl))).Execute(website, map[string]interface{}{
		"Network":   *netnameFlag,
		"Amounts":   amounts,
		"Periods":   periods,
		"Recaptcha": *captchaToken,
	})
	if err != nil {
		log.Crit("Failed to render the faucet template", "err", err)
	}
	// Load and parse the genesis block requested by the user
	blob, err := ioutil.ReadFile(*genesisFlag)
	if err != nil {
		log.Crit("Failed to read genesis block contents", "genesis", *genesisFlag, "err", err)
	}
	genesis := new(core.Genesis)
	if err = json.Unmarshal(blob, genesis); err != nil {
		log.Crit("Failed to parse genesis block json", "err", err)
	}
	// Convert the bootnodes to internal enode representations
	var enodes []*discv5.Node
	for _, boot := range strings.Split(*bootFlag, ",") {
		if url, err := discv5.ParseNode(boot); err == nil {
			enodes = append(enodes, url)
		} else {
			log.Error("Failed to parse bootnode URL", "url", boot, "err", err)
		}
	}
	// Load up the account key and decrypt its password
	if blob, err = ioutil.ReadFile(*accPassFlag); err != nil {
		log.Crit("Failed to read account password contents", "file", *accPassFlag, "err", err)
	}
	pass := string(blob)

	ks := keystore.NewKeyStore(filepath.Join(os.Getenv("HOME"), ".faucet", "keys"), keystore.StandardScryptN, keystore.StandardScryptP)
	if blob, err = ioutil.ReadFile(*accJSONFlag); err != nil {
		log.Crit("Failed to read account key contents", "file", *accJSONFlag, "err", err)
	}
	acc, err := ks.Import(blob, pass, pass)
	if err != nil {
		log.Crit("Failed to import faucet signer account", "err", err)
	}
	ks.Unlock(acc, pass)

	// Assemble and start the faucet light service
	faucet, err := newFaucet(genesis, *ethPortFlag, enodes, *netFlag, *statsFlag, ks, website.Bytes())
	if err != nil {
		log.Crit("Failed to start faucet", "err", err)
	}
	defer faucet.close()

	if err := faucet.listenAndServe(*apiPortFlag); err != nil {
		log.Crit("Failed to launch faucet API", "err", err)
	}
}

// request represents an accepted funding request.
type request struct {
	Username string             `json:"username"` // GitHub user for displaying an avatar
	Account  common.Address     `json:"account"`  // Ethereum address being funded
	Time     time.Time          `json:"time"`     // Timestamp when te request was accepted
	Tx       *types.Transaction `json:"tx"`       // Transaction funding the account
}

// faucet represents a crypto faucet backed by an Ethereum light client.
type faucet struct {
	config *params.ChainConfig // Chain configurations for signing
	stack  *node.Node          // Ethereum protocol stack
	client *ethclient.Client   // Client connection to the Ethereum chain
	index  []byte              // Index page to serve up on the web

	keystore *keystore.KeyStore // Keystore containing the single signer
	account  accounts.Account   // Account funding user faucet requests
	nonce    uint64             // Current pending nonce of the faucet
	price    *big.Int           // Current gas price to issue funds with

	conns    []*websocket.Conn    // Currently live websocket connections
	timeouts map[string]time.Time // History of users and their funding timeouts
	reqs     []*request           // Currently pending funding requests
	update   chan struct{}        // Channel to signal request updates

	lock sync.RWMutex // Lock protecting the faucet's internals
}

func newFaucet(genesis *core.Genesis, port int, enodes []*discv5.Node, network uint64, stats string, ks *keystore.KeyStore, index []byte) (*faucet, error) {
	// Assemble the raw devp2p protocol stack
	stack, err := node.New(&node.Config{
		Name:    "bith",
		Version: params.Version,
		DataDir: filepath.Join(os.Getenv("HOME"), ".faucet"),
		P2P: p2p.Config{
			NAT:              nat.Any(),
			NoDiscovery:      true,
			DiscoveryV5:      true,
			ListenAddr:       fmt.Sprintf(":%d", port),
			DiscoveryV5Addr:  fmt.Sprintf(":%d", port+1),
			MaxPeers:         25,
			BootstrapNodesV5: enodes,
		},
	})
	if err != nil {
		return nil, err
	}
	// Assemble the Ethereum light client protocol
	if err := stack.Register(func(ctx *node.ServiceContext) (node.Service, error) {
		cfg := eth.DefaultConfig
		cfg.SyncMode = downloader.LightSync
		cfg.NetworkId = network
		cfg.Genesis = genesis
		return les.New(ctx, &cfg)
	}); err != nil {
		return nil, err
	}
	// Assemble the ethstats monitoring and reporting service'
	if stats != "" {
		if err := stack.Register(func(ctx *node.ServiceContext) (node.Service, error) {
			var serv *les.LightEthereum
			ctx.Service(&serv)
			return ethstats.New(stats, nil, serv)
		}); err != nil {
			return nil, err
		}
	}
	// Boot up the client and ensure it connects to bootnodes
	if err := stack.Start(); err != nil {
		return nil, err
	}
	for _, boot := range enodes {
		old, _ := discover.ParseNode(boot.String())
		stack.Server().AddPeer(old)
	}
	// Attach to the client and retrieve and interesting metadatas
	api, err := stack.Attach()
	if err != nil {
		stack.Stop()
		return nil, err
	}
	client := ethclient.NewClient(api)

	return &faucet{
		config:   genesis.Config,
		stack:    stack,
		client:   client,
		index:    index,
		keystore: ks,
		account:  ks.Accounts()[0],
		timeouts: make(map[string]time.Time),
		update:   make(chan struct{}, 1),
	}, nil
}

// close terminates the Ethereum connection and tears down the faucet.
func (f *faucet) close() error {
	return f.stack.Stop()
}

// listenAndServe registers the HTTP handlers for the faucet and boots it up
// for service user funding requests.
func (f *faucet) listenAndServe(port int) error {
	go f.loop()

	http.HandleFunc("/", f.webHandler)
	http.Handle("/api", websocket.Handler(f.apiHandler))

	return http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
}

// webHandler handles all non-api requests, simply flattening and returning the
// faucet website.
func (f *faucet) webHandler(w http.ResponseWriter, r *http.Request) {
	w.Write(f.index)
}

// apiHandler handles requests for Ether grants and transaction statuses.
func (f *faucet) apiHandler(conn *websocket.Conn) {
	// Start tracking the connection and drop at the end
	f.lock.Lock()
	f.conns = append(f.conns, conn)
	f.lock.Unlock()

	defer func() {
		f.lock.Lock()
		for i, c := range f.conns {
			if c == conn {
				f.conns = append(f.conns[:i], f.conns[i+1:]...)
				break
			}
		}
		f.lock.Unlock()
	}()
	// Send a few initial stats to the client
	balance, _ := f.client.BalanceAt(context.Background(), f.account.Address, nil)
	nonce, _ := f.client.NonceAt(context.Background(), f.account.Address, nil)

	websocket.JSON.Send(conn, map[string]interface{}{
		"funds":    balance.Div(balance, ether),
		"funded":   nonce,
		"peers":    f.stack.Server().PeerCount(),
		"requests": f.reqs,
	})
	// Send the initial block to the client
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	header, err := f.client.HeaderByNumber(ctx, nil)
	cancel()

	if err != nil {
		log.Error("Failed to retrieve latest header", "err", err)
	} else {
		websocket.JSON.Send(conn, header)
	}
	// Keep reading requests from the websocket until the connection breaks
	for {
		// Fetch the next funding request and validate against github
		var msg struct {
			URL     string `json:"url"`
			Tier    uint   `json:"tier"`
			Captcha string `json:"captcha"`
		}
		if err := websocket.JSON.Receive(conn, &msg); err != nil {
			return
		}
		if !strings.HasPrefix(msg.URL, "https://gist.github.com/") {
			websocket.JSON.Send(conn, map[string]string{"error": "URL doesn't link to GitHub Gists"})
			continue
		}
		if msg.Tier >= uint(*tiersFlag) {
			websocket.JSON.Send(conn, map[string]string{"error": "Invalid funding tier requested"})
			continue
		}
		log.Info("Faucet funds requested", "gist", msg.URL, "tier", msg.Tier)

		// If captcha verifications are enabled, make sure we're not dealing with a robot
		if *captchaToken != "" {
			form := url.Values{}
			form.Add("secret", *captchaSecret)
			form.Add("response", msg.Captcha)

			res, err := http.PostForm("https://www.google.com/recaptcha/api/siteverify", form)
			if err != nil {
				websocket.JSON.Send(conn, map[string]string{"error": err.Error()})
				continue
			}
			var result struct {
				Success bool            `json:"success"`
				Errors  json.RawMessage `json:"error-codes"`
			}
			err = json.NewDecoder(res.Body).Decode(&result)
			res.Body.Close()
			if err != nil {
				websocket.JSON.Send(conn, map[string]string{"error": err.Error()})
				continue
			}
			if !result.Success {
				log.Warn("Captcha verification failed", "err", string(result.Errors))
				websocket.JSON.Send(conn, map[string]string{"error": "Beep-bop, you're a robot!"})
				continue
			}
		}
		// Retrieve the gist from the GitHub Gist APIs
		parts := strings.Split(msg.URL, "/")
		req, _ := http.NewRequest("GET", "https://api.github.com/gists/"+parts[len(parts)-1], nil)
		if *githubUser != "" {
			req.SetBasicAuth(*githubUser, *githubToken)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			websocket.JSON.Send(conn, map[string]string{"error": err.Error()})
			continue
		}
		var gist struct {
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
			Files map[string]struct {
				Content string `json:"content"`
			} `json:"files"`
		}
		err = json.NewDecoder(res.Body).Decode(&gist)
		res.Body.Close()
		if err != nil {
			websocket.JSON.Send(conn, map[string]string{"error": err.Error()})
			continue
		}
		if gist.Owner.Login == "" {
			websocket.JSON.Send(conn, map[string]string{"error": "Anonymous Gists not allowed"})
			continue
		}
		// Iterate over all the files and look for Ethereum addresses
		var address common.Address
		for _, file := range gist.Files {
			content := strings.TrimSpace(file.Content)
			if len(content) == 2+common.AddressLength*2 {
				address = common.HexToAddress(content)
			}
		}
		if address == (common.Address{}) {
			websocket.JSON.Send(conn, map[string]string{"error": "No Ethereum address found to fund"})
			continue
		}
		// Validate the user's existence since the API is unhelpful here
		if res, err = http.Head("https://github.com/" + gist.Owner.Login); err != nil {
			websocket.JSON.Send(conn, map[string]string{"error": err.Error()})
			continue
		}
		res.Body.Close()

		if res.StatusCode != 200 {
			websocket.JSON.Send(conn, map[string]string{"error": "Invalid user... boom!"})
			continue
		}
		// Ensure the user didn't request funds too recently
		f.lock.Lock()
		var (
			fund    bool
			timeout time.Time
		)
		if timeout = f.timeouts[gist.Owner.Login]; time.Now().After(timeout) {
			// User wasn't funded recently, create the funding transaction
			amount := new(big.Int).Mul(big.NewInt(int64(*payoutFlag)), ether)
			amount = new(big.Int).Mul(amount, new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(msg.Tier)), nil))
			amount = new(big.Int).Div(amount, new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(msg.Tier)), nil))

			tx := types.NewTransaction(f.nonce+uint64(len(f.reqs)), address, amount, big.NewInt(21000), f.price, nil)
			signed, err := f.keystore.SignTx(f.account, tx, f.config.ChainId)
			if err != nil {
				websocket.JSON.Send(conn, map[string]string{"error": err.Error()})
				f.lock.Unlock()
				continue
			}
			// Submit the transaction and mark as funded if successful
			if err := f.client.SendTransaction(context.Background(), signed); err != nil {
				websocket.JSON.Send(conn, map[string]string{"error": err.Error()})
				f.lock.Unlock()
				continue
			}
			f.reqs = append(f.reqs, &request{
				Username: gist.Owner.Login,
				Account:  address,
				Time:     time.Now(),
				Tx:       signed,
			})
			f.timeouts[gist.Owner.Login] = time.Now().Add(time.Duration(*minutesFlag*int(math.Pow(3, float64(msg.Tier)))) * time.Minute)
			fund = true
		}
		f.lock.Unlock()

		// Send an error if too frequent funding, othewise a success
		if !fund {
			websocket.JSON.Send(conn, map[string]string{"error": fmt.Sprintf("%s left until next allowance", common.PrettyDuration(timeout.Sub(time.Now())))})
			continue
		}
		websocket.JSON.Send(conn, map[string]string{"success": fmt.Sprintf("Funding request accepted for %s into %s", gist.Owner.Login, address.Hex())})
		select {
		case f.update <- struct{}{}:
		default:
		}
	}
}

// loop keeps waiting for interesting events and pushes them out to connected
// websockets.
func (f *faucet) loop() {
	// Wait for chain events and push them to clients
	heads := make(chan *types.Header, 16)
	sub, err := f.client.SubscribeNewHead(context.Background(), heads)
	if err != nil {
		log.Crit("Failed to subscribe to head events", "err", err)
	}
	defer sub.Unsubscribe()

	for {
		select {
		case head := <-heads:
			// New chain head arrived, query the current stats and stream to clients
			balance, _ := f.client.BalanceAt(context.Background(), f.account.Address, nil)
			balance = new(big.Int).Div(balance, ether)

			price, _ := f.client.SuggestGasPrice(context.Background())
			nonce, _ := f.client.NonceAt(context.Background(), f.account.Address, nil)

			f.lock.Lock()
			f.price, f.nonce = price, nonce
			for len(f.reqs) > 0 && f.reqs[0].Tx.Nonce() < f.nonce {
				f.reqs = f.reqs[1:]
			}
			f.lock.Unlock()

			f.lock.RLock()
			for _, conn := range f.conns {
				if err := websocket.JSON.Send(conn, map[string]interface{}{
					"funds":    balance,
					"funded":   f.nonce,
					"peers":    f.stack.Server().PeerCount(),
					"requests": f.reqs,
				}); err != nil {
					log.Warn("Failed to send stats to client", "err", err)
					conn.Close()
					continue
				}
				if err := websocket.JSON.Send(conn, head); err != nil {
					log.Warn("Failed to send header to client", "err", err)
					conn.Close()
				}
			}
			f.lock.RUnlock()

		case <-f.update:
			// Pending requests updated, stream to clients
			f.lock.RLock()
			for _, conn := range f.conns {
				if err := websocket.JSON.Send(conn, map[string]interface{}{"requests": f.reqs}); err != nil {
					log.Warn("Failed to send requests to client", "err", err)
					conn.Close()
				}
			}
			f.lock.RUnlock()
		}
	}
}
