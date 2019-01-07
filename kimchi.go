// kimchi.go - Katzenpost self contained test network.
// Copyright (C) 2017  Yawning Angel, David Stainton, Masala.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package kimchi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/textproto"
	"os"
	"path/filepath"
	"sync"

	"github.com/hpcloud/tail"
	vServer "github.com/katzenpost/authority/voting/server"
	vConfig "github.com/katzenpost/authority/voting/server/config"
	aServer "github.com/katzenpost/authority/nonvoting/server"
	aConfig "github.com/katzenpost/authority/nonvoting/server/config"
	"github.com/katzenpost/core/crypto/ecdh"
	"github.com/katzenpost/core/crypto/eddsa"
	"github.com/katzenpost/core/crypto/rand"
	"github.com/katzenpost/core/thwack"
	"github.com/katzenpost/mailproxy"
	pConfig "github.com/katzenpost/mailproxy/config"
	"github.com/katzenpost/mailproxy/event"
	nServer "github.com/katzenpost/server"
	sConfig "github.com/katzenpost/server/config"
)

const (
	logFile       = "kimchi.log"
	basePort      = 30000
)

var tailConfig = tail.Config{
	Poll:   true,
	Follow: true,
	Logger: tail.DiscardingLogger,
}

type kimchi struct {
	sync.Mutex
	sync.WaitGroup

	baseDir   string
	logWriter io.Writer

	authConfig    *aConfig.Config
	votingAuthConfigs []*vConfig.Config
	authIdentity  *eddsa.PrivateKey
	voting bool

	nVoting   int
	nProvider int
	nMix      int

	nodeConfigs []*sConfig.Config
	lastPort    uint16
	nodeIdx     int
	providerIdx int

	recipients map[string]*ecdh.PublicKey

	servers []server
	tails   []*tail.Tail
}

type server interface {
	Shutdown()
	Wait()
}

func NewKimchi(basePort int, baseDir string, voting bool, nVoting, nProvider, nMix int) *kimchi {
	k := &kimchi{
		lastPort:    uint16(basePort + 1),
		recipients:  make(map[string]*ecdh.PublicKey),
		nodeConfigs: make([]*sConfig.Config, 0),
		voting:      voting,
		nVoting:     nVoting,
		nProvider:   nProvider,
		nMix:        nMix,
	}
	// Create the base directory and bring logging online.
	var err error
	if baseDir == "" {
		k.baseDir, err = ioutil.TempDir("", "kimchi")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create base directory: %v\n", err)
			os.Exit(-1)
		}
	} else {
		k.baseDir = baseDir
	}
	if err = k.initLogging(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logging: %v\n", err)
		os.Exit(-1)
	}
	if err = k.initConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initConfig(): %v", err)
		return nil
	}
	return k
}

func (k *kimchi) Run() {
	// Launch all the nodes.
	for _, v := range k.nodeConfigs {
		v.FixupAndValidate()
		svr, err := nServer.New(v)
		if err != nil {
			log.Fatalf("Failed to launch node: %v", err)
		}

		k.servers = append(k.servers, svr)
		go k.logTailer(v.Server.Identifier, filepath.Join(v.Server.DataDir, v.Logging.File))
	}
	k.runAuthority()
}

func (k *kimchi) initConfig() error {
	// Generate the authority configs
	var err error
	if k.voting {
		err = k.genVotingAuthoritiesCfg()
		if err != nil {
			log.Fatalf("getVotingAuthoritiesCfg failed: %s", err)
		}
	} else {
		if err = k.genAuthConfig(); err != nil {
			log.Fatalf("Failed to generate authority config: %v", err)
		}
	}

	// Generate the provider configs.
	for i := 0; i < k.nProvider; i++ {
		if err = k.genNodeConfig(true, k.voting); err != nil {
			log.Fatalf("Failed to generate provider config: %v", err)
		}
	}

	// Generate the node configs.
	for i := 0; i < k.nMix; i++ {
		if err = k.genNodeConfig(false, k.voting); err != nil {
			log.Fatalf("Failed to generate node config: %v", err)
		}
	}

	// Generate the node lists.
	if k.voting {
		providerWhitelist, mixWhitelist, err := k.generateVotingWhitelist()
		if err != nil {
			panic(err)
		}
		for _, aCfg := range k.votingAuthConfigs {
			aCfg.Mixes = mixWhitelist
			aCfg.Providers = providerWhitelist
		}
	} else {
		if providers, mixes, err := k.generateWhitelist(); err == nil {
			k.authConfig.Mixes = mixes
			k.authConfig.Providers = providers
		} else {
			log.Fatalf("Failed to generateWhitelist with %s", err)
		}
	}
	return err
}

func (k *kimchi) runAuthority() {
	var err error
	if k.voting {
		err = k.runVotingAuthorities()
	} else {
		err = k.runNonvoting()
	}
	if err != nil {
		panic(err)
	}
}

func (k *kimchi) initLogging() error {
	logFilePath := filepath.Join(k.baseDir, logFile)
	f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}

	// Log to both stdout *and* the log file.
	k.logWriter = io.MultiWriter(f, os.Stdout)
	log.SetOutput(k.logWriter)

	return nil
}

func (k *kimchi) genVotingAuthoritiesCfg() error {
	parameters := &vConfig.Parameters{
		MixLambda:       1,
		MixMaxDelay:     10000,
		SendLambda:      123,
		SendMaxInterval: 123456,
	}
	configs := []*vConfig.Config{}

	// initial generation of key material for each authority
	peersMap := make(map[[eddsa.PublicKeySize]byte]*vConfig.AuthorityPeer)
	for i := 0; i < k.nVoting; i++ {
		cfg := new(vConfig.Config)
		cfg.Logging = &vConfig.Logging{
			Disable: false,
			File:    "katzenpost.log",
			Level:   "DEBUG",
		}
		cfg.Parameters = parameters
		cfg.Authority = &vConfig.Authority{
			Identifier: fmt.Sprintf("authority-%v.example.org", i),
			Addresses:  []string{fmt.Sprintf("127.0.0.1:%d", k.lastPort)},
			DataDir:    filepath.Join(k.baseDir, fmt.Sprintf("authority%d", i)),
		}
		k.lastPort += 1
		if err := os.Mkdir(cfg.Authority.DataDir, 0700); err != nil {
			return err
		}
		idKey, err := eddsa.NewKeypair(rand.Reader)
		if err != nil {
			return err
		}

		if err != nil {
			return err
		}
		cfg.Debug = &vConfig.Debug{
			IdentityKey:      idKey,
			LinkKey:          idKey.ToECDH(),
			Layers:           3,
			MinNodesPerLayer: 1,
			GenerateOnly:     false,
		}
		configs = append(configs, cfg)
		authorityPeer := &vConfig.AuthorityPeer{
			IdentityPublicKey: cfg.Debug.IdentityKey.PublicKey(),
			LinkPublicKey:     cfg.Debug.LinkKey.PublicKey(),
			Addresses:         cfg.Authority.Addresses,
		}
		peersMap[cfg.Debug.IdentityKey.PublicKey().ByteArray()] = authorityPeer
	}

	// tell each authority about it's peers
	for i := 0; i < k.nVoting; i++ {
		peers := []*vConfig.AuthorityPeer{}
		for id, peer := range peersMap {
			if !bytes.Equal(id[:], configs[i].Debug.IdentityKey.PublicKey().Bytes()) {
				peers = append(peers, peer)
			}
		}
		configs[i].Authorities = peers
	}
	k.votingAuthConfigs = configs
	return nil
}

func (k *kimchi) genNodeConfig(isProvider bool, isVoting bool) error {
	const serverLogFile = "katzenpost.log"

	n := fmt.Sprintf("node-%d", k.nodeIdx)
	if isProvider {
		n = fmt.Sprintf("provider-%d", k.providerIdx)
	}
	cfg := new(sConfig.Config)

	// Server section.
	cfg.Server = new(sConfig.Server)
	cfg.Server.Identifier = fmt.Sprintf("%s.eXaMpLe.org", n)
	cfg.Server.Addresses = []string{fmt.Sprintf("127.0.0.1:%d", k.lastPort)}
	cfg.Server.DataDir = filepath.Join(k.baseDir, n)
	cfg.Server.IsProvider = isProvider

	// Logging section.
	cfg.Logging = new(sConfig.Logging)
	cfg.Logging.File = serverLogFile
	cfg.Logging.Level = "DEBUG"

	// Debug section.
	cfg.Debug = new(sConfig.Debug)
	cfg.Debug.NumSphinxWorkers = 1
	identity, err := eddsa.NewKeypair(rand.Reader)
	if err != nil {
		return err
	}
	cfg.Debug.IdentityKey = identity

	if isVoting {
		peers := []*sConfig.Peer{}
		for _, peer := range k.votingAuthConfigs {
			idKey, err := peer.Debug.IdentityKey.PublicKey().MarshalText()
			if err != nil {
				return err
			}
			linkKey, err := peer.Debug.LinkKey.PublicKey().MarshalText()
			if err != nil {
				return err
			}
			p := &sConfig.Peer{
				Addresses:         peer.Authority.Addresses,
				IdentityPublicKey: string(idKey),
				LinkPublicKey: string(linkKey),
			}
			if len(peer.Authority.Addresses) == 0 {
				panic("wtf")
			}
			peers = append(peers, p)
		}
		cfg.PKI = &sConfig.PKI{
			Voting: &sConfig.Voting{
				Peers: peers,
			},
		}
	} else {
		cfg.PKI = new(sConfig.PKI)
		cfg.PKI.Nonvoting = new(sConfig.Nonvoting)
		cfg.PKI.Nonvoting.Address = fmt.Sprintf("127.0.0.1:%d", basePort)
		if k.authIdentity == nil {
		}
		idKey, err := k.authIdentity.PublicKey().MarshalText()
		if err != nil {
			return err
		}
		cfg.PKI.Nonvoting.PublicKey = string(idKey)
	}

	if isProvider {
		// Enable the thwack interface.
		cfg.Management = new(sConfig.Management)
		cfg.Management.Enable = true

		k.providerIdx++

		cfg.Provider = new(sConfig.Provider)

		loopCfg := new(sConfig.Kaetzchen)
		loopCfg.Capability = "loop"
		loopCfg.Endpoint = "+loop"
		cfg.Provider.Kaetzchen = append(cfg.Provider.Kaetzchen, loopCfg)

		keysvrCfg := new(sConfig.Kaetzchen)
		keysvrCfg.Capability = "keyserver"
		keysvrCfg.Endpoint = "+keyserver"
		cfg.Provider.Kaetzchen = append(cfg.Provider.Kaetzchen, keysvrCfg)

		/*
			if s.providerIdx == 1 {
				cfg.Debug.NumProviderWorkers = 10
				cfg.Provider.SQLDB = new(sConfig.SQLDB)
				cfg.Provider.SQLDB.Backend = "pgx"
				cfg.Provider.SQLDB.DataSourceName = "host=localhost port=5432 database=katzenpost sslmode=disable"
				cfg.Provider.UserDB = new(sConfig.UserDB)
				cfg.Provider.UserDB.Backend = sConfig.BackendSQL

				cfg.Provider.SpoolDB = new(sConfig.SpoolDB)
				cfg.Provider.SpoolDB.Backend = sConfig.BackendSQL
			}
		*/
	} else {
		k.nodeIdx++
	}
	k.nodeConfigs = append(k.nodeConfigs, cfg)
	k.lastPort++
	err = cfg.FixupAndValidate()
	if err != nil {
		return errors.New("genNodeConfig failure on fixupandvalidate")
	}
	return nil
}

func (k *kimchi) genAuthConfig() error {
	const authLogFile = "authority.log"

	cfg := new(aConfig.Config)

	// Authority section.
	cfg.Authority = new(aConfig.Authority)
	cfg.Authority.Addresses = []string{fmt.Sprintf("127.0.0.1:%d", basePort)}
	cfg.Authority.DataDir = filepath.Join(k.baseDir, "authority")

	// Logging section.
	cfg.Logging = new(aConfig.Logging)
	cfg.Logging.File = authLogFile
	cfg.Logging.Level = "DEBUG"

	// Mkdir
	if err := os.Mkdir(cfg.Authority.DataDir, 0700); err != nil {
		return err
	}

	// Generate Keys
	idKey, err := eddsa.NewKeypair(rand.Reader)
	k.authIdentity = idKey
	if err != nil {
		return err
	}

	// Debug section.
	cfg.Debug = new(aConfig.Debug)
	cfg.Debug.IdentityKey = idKey

	if err := cfg.FixupAndValidate(); err != nil {
		return err
	}
	k.authConfig = cfg
	return nil
}

func (k *kimchi) generateWhitelist() ([]*aConfig.Node, []*aConfig.Node, error) {
	mixes := []*aConfig.Node{}
	providers := []*aConfig.Node{}
	for _, nodeCfg := range k.nodeConfigs {
		if nodeCfg.Server.IsProvider {
			provider := &aConfig.Node{
				Identifier:  nodeCfg.Server.Identifier,
				IdentityKey: nodeCfg.Debug.IdentityKey.PublicKey(),
			}
			providers = append(providers, provider)
			continue
		}
		mix := &aConfig.Node{
			IdentityKey: nodeCfg.Debug.IdentityKey.PublicKey(),
		}
		mixes = append(mixes, mix)
	}

	return providers, mixes, nil

}

// generateWhitelist returns providers, mixes, error
func (k *kimchi) generateVotingWhitelist() ([]*vConfig.Node, []*vConfig.Node, error) {
	mixes := []*vConfig.Node{}
	providers := []*vConfig.Node{}
	for _, nodeCfg := range k.nodeConfigs {
		if nodeCfg.Server.IsProvider {
			provider := &vConfig.Node{
				Identifier:  nodeCfg.Server.Identifier,
				IdentityKey: nodeCfg.Debug.IdentityKey.PublicKey(),
			}
			providers = append(providers, provider)
			continue
		}
		mix := &vConfig.Node{
			IdentityKey: nodeCfg.Debug.IdentityKey.PublicKey(),
		}
		mixes = append(mixes, mix)
	}

	return providers, mixes, nil
}

func (k *kimchi) runNonvoting() error {
	a := k.authConfig
	a.FixupAndValidate()
	server, err := aServer.New(a)
	if err != nil {
		return err
	}
	go k.logTailer("nonvoting", filepath.Join(a.Authority.DataDir, a.Logging.File))
	k.servers = append(k.servers, server)
	return nil
}

func (k *kimchi) runVotingAuthorities() error {
	for _, vCfg := range k.votingAuthConfigs {
		vCfg.FixupAndValidate()
		server, err := vServer.New(vCfg)
		if err != nil {
			return err
		}
		go k.logTailer(vCfg.Authority.Identifier, filepath.Join(vCfg.Authority.DataDir, vCfg.Logging.File))
		k.servers = append(k.servers, server)
	}
	return nil
}

func (k *kimchi) newMailProxy(user, provider string, privateKey *ecdh.PrivateKey, isVoting bool) (*mailproxy.Proxy, error) {
	const (
		proxyLogFile = "katzenpost.log"
		authID       = "testAuth"
	)

	cfg := new(pConfig.Config)

	dispName := fmt.Sprintf("mailproxy-%v@%v", user, provider)

	// Proxy section.
	cfg.Proxy = new(pConfig.Proxy)
	cfg.Proxy.POP3Address = fmt.Sprintf("127.0.0.1:%d", k.lastPort)
	k.lastPort++
	cfg.Proxy.SMTPAddress = fmt.Sprintf("127.0.0.1:%d", k.lastPort)
	k.lastPort++
	cfg.Proxy.DataDir = filepath.Join(k.baseDir, dispName)

	// Logging section.
	cfg.Logging = new(pConfig.Logging)
	cfg.Logging.File = proxyLogFile
	cfg.Logging.Level = "DEBUG"

	// Management section.
	cfg.Management = new(pConfig.Management)
	cfg.Management.Enable = true

	// Account section.
	acc := new(pConfig.Account)
	acc.User = user
	acc.Provider = provider
	acc.LinkKey = privateKey
	acc.IdentityKey = privateKey
	// acc.StorageKey = privateKey
	cfg.Account = append(cfg.Account, acc)

	// UpstreamProxy section.
	/*
		cfg.UpstreamProxy = new(pConfig.UpstreamProxy)
		cfg.UpstreamProxy.Type = "tor+socks5"
		// cfg.UpstreamProxy.Network = "unix"
		// cfg.UpstreamProxy.Address = "/tmp/socks.socket"
		cfg.UpstreamProxy.Network = "tcp"
		cfg.UpstreamProxy.Address = "127.0.0.1:1080"
	*/

	// Recipients section.
	cfg.Recipients = k.recipients

	if err := cfg.FixupAndValidate(); err != nil {
		return nil, err
	}

	p, err := mailproxy.New(cfg)
	if err != nil {
		return nil, err
	}

	go func() {
		for ev := range p.EventSink {
			log.Printf("%v: Event: %+v", dispName, ev)
			switch e := ev.(type) {
			case *event.KaetzchenReplyEvent:
				// Just assume this is a keyserver query for now.
				if u, k, err := p.ParseKeyQueryResponse(e.Payload); err != nil {
					log.Printf("%v: Keyserver query failed: %v", dispName, err)
				} else {
					log.Printf("%v: Keyserver reply: %v -> %v", dispName, u, k)
				}
			default:
			}
		}
	}()

	go k.logTailer(dispName, filepath.Join(cfg.Proxy.DataDir, proxyLogFile))

	return p, nil
}

func (k *kimchi) thwackUser(provider *sConfig.Config, user string, pubKey *ecdh.PublicKey) error {
	log.Printf("Attempting to add user: %v@%v", user, provider.Server.Identifier)

	sockFn := filepath.Join(provider.Server.DataDir, "management_sock")
	c, err := textproto.Dial("unix", sockFn)
	if err != nil {
		return err
	}
	defer c.Close()

	if _, _, err = c.ReadResponse(int(thwack.StatusServiceReady)); err != nil {
		return err
	}

	for _, v := range []string{
		fmt.Sprintf("ADD_USER %v %v", user, pubKey),
		fmt.Sprintf("SET_USER_IDENTITY %v %v", user, pubKey),
		"QUIT",
	} {
		if err = c.PrintfLine("%v", v); err != nil {
			return err
		}
		if _, _, err = c.ReadResponse(int(thwack.StatusOk)); err != nil {
			return err
		}
	}

	return nil
}

func (k *kimchi) logTailer(prefix, path string) {
	k.Add(1)
	defer k.Done()

	l := log.New(k.logWriter, prefix+" ", 0)
	t, err := tail.TailFile(path, tailConfig)
	defer t.Cleanup()
	if err != nil {
		log.Fatalf("Failed to tail file '%v': %v", path, err)
	}

	k.Lock()
	k.tails = append(k.tails, t)
	k.Unlock()

	for line := range t.Lines {
		l.Print(line.Text)
	}
}

func (k *kimchi) Shutdown() {
	for _, svr := range k.servers {
		svr.Shutdown()
	}
	for _, t := range k.tails {
		t.StopAtEOF()
	}
	k.Wait()
	log.Printf("Terminated.")
}
