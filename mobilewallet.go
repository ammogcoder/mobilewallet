package mobilewallet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/decred/dcrd/addrmgr"
	stake "github.com/decred/dcrd/blockchain/stake"
	"github.com/decred/dcrd/chaincfg"
	chainhash "github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrjson"
	"github.com/decred/dcrd/dcrutil"
	"github.com/decred/dcrd/hdkeychain"
	"github.com/decred/dcrd/txscript"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrwallet/chain"
	"github.com/decred/dcrwallet/errors"
	"github.com/decred/dcrwallet/loader"
	"github.com/decred/dcrwallet/netparams"
	"github.com/decred/dcrwallet/p2p"
	"github.com/decred/dcrwallet/spv"
	"github.com/decred/dcrwallet/wallet"
	"github.com/decred/dcrwallet/wallet/txrules"
	walletseed "github.com/decred/dcrwallet/walletseed"
	"github.com/decred/slog"
)

var shutdownRequestChannel = make(chan struct{})
var shutdownSignaled = make(chan struct{})
var signals = []os.Signal{os.Interrupt}

type LibWallet struct {
	dataDir     string
	dbDriver    string
	wallet      *wallet.Wallet
	rpcClient   *chain.RPCClient
	spvSyncer   *spv.Syncer
	loader      *loader.Loader
	netBackend  wallet.NetworkBackend
	mu          sync.Mutex
	activeNet   *netparams.Params
	chainParams *chaincfg.Params
	lock        chan time.Time
}

func NewLibWallet(homeDir string, dbDriver string) *LibWallet {
	lw := &LibWallet{
		dataDir:  filepath.Join(homeDir, "testnet3/"),
		dbDriver: dbDriver,
	}
	errors.Separator = ":: "
	initLogRotator(filepath.Join(homeDir, "/logs/testnet3/dcrwallet.log"))
	log.Info("GC PERCENT:", debug.SetGCPercent(100))
	return lw
}

func (lw *LibWallet) SetLogLevel(loglevel string) {
	_, ok := slog.LevelFromString(loglevel)
	if ok {
		setLogLevels(loglevel)
	}
}

func NormalizeAddress(addr string, defaultPort string) (hostport string, err error) {
	// If the first SplitHostPort errors because of a missing port and not
	// for an invalid host, add the port.  If the second SplitHostPort
	// fails, then a port is not missing and the original error should be
	// returned.
	host, port, origErr := net.SplitHostPort(addr)
	if origErr == nil {
		return net.JoinHostPort(host, port), nil
	}
	addr = net.JoinHostPort(addr, defaultPort)
	_, _, err = net.SplitHostPort(addr)
	if err != nil {
		return "", origErr
	}
	return addr, nil
}

func (lw *LibWallet) UnlockWallet(privPass []byte) error {
	if lw.lock != nil {
		//Wallet is unlocked
		return nil
	}
	wallet, ok := lw.loader.LoadedWallet()
	if !ok {
		return fmt.Errorf("Wallet has not been loaded")
	}
	defer func() {
		for i := range privPass {
			privPass[i] = 0
		}
	}()
	lw.lock = make(chan time.Time, 1)
	err := wallet.Unlock(privPass, lw.lock)
	return err
}

func (lw *LibWallet) LockWallet() {
	if lw.lock == nil {
		return
	}
	lw.lock <- time.Time{}
}

func (lw *LibWallet) Shutdown() {
	log.Info("Shuting down mobile wallet")
	shutdownRequestChannel <- struct{}{}
	lw.LockWallet()
	err := lw.loader.UnloadWallet()
	if err != nil {
		log.Errorf("Failed to close wallet: %v", err)
	} else {
		log.Infof("Closed wallet")
	}
	if logRotator != nil {
		log.Infof("Shutting down log rotator")
		logRotator.Close()
	}
	os.Exit(0)
}

func shutdownListener() {
	interruptChannel := make(chan os.Signal, 1)
	signal.Notify(interruptChannel, signals...)

	// Listen for the initial shutdown signal
	select {
	case sig := <-interruptChannel:
		log.Infof("Received signal (%s).  Shutting down...", sig)
	case <-shutdownRequestChannel:
		log.Info("Shutdown requested.  Shutting down...")
	}

	// Cancel all contexts created from withShutdownCancel.
	close(shutdownSignaled)

	// Listen for any more shutdown signals and log that shutdown has already
	// been signaled.
	for {
		select {
		case <-interruptChannel:
		case <-shutdownRequestChannel:
		}
		log.Info("Shutdown signaled.  Already shutting down...")
	}
}

func contextWithShutdownCancel(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		<-shutdownSignaled
		cancel()
	}()
	return ctx
}

func decodeAddress(a string, params *chaincfg.Params) (dcrutil.Address, error) {
	addr, err := dcrutil.DecodeAddress(a)
	if err != nil {
		return nil, err
	}
	if !addr.IsForNet(params) {
		return nil, fmt.Errorf("address %v is not intended for use on %v",
			a, params.Name)
	}
	return addr, nil
}

func (lw *LibWallet) InitLoader() {
	stakeOptions := &loader.StakeOptions{
		VotingEnabled: false,
		AddressReuse:  false,
		VotingAddress: nil,
		TicketFee:     10e8,
	}
	l := loader.NewLoader(netparams.TestNet3Params.Params, lw.dataDir, lw.dbDriver, stakeOptions,
		20, false, 10e5, wallet.DefaultAccountGapLimit)
	lw.loader = l
	lw.activeNet = &netparams.TestNet3Params
	lw.chainParams = &chaincfg.TestNet3Params
	go shutdownListener()
}

func (lw *LibWallet) CreateWallet(passphrase string, seedMnemonic string) error {
	fmt.Println("Creating wallet")
	pubPass := []byte(wallet.InsecurePubPassphrase)
	privPass := []byte(passphrase)
	seed, err := walletseed.DecodeUserInput(seedMnemonic)
	if err != nil {
		log.Error(err)
		return err
	}

	w, err := lw.loader.CreateNewWallet(pubPass, privPass, seed)
	if err != nil {
		log.Error(err)
		return err
	}
	lw.wallet = w

	fmt.Println("Created Wallet")
	return nil
}

func (lw *LibWallet) CloseWallet() error {
	err := lw.loader.UnloadWallet()
	return err
}

func (lw *LibWallet) GenerateSeed() (string, error) {
	seed, err := hdkeychain.GenerateSeed(hdkeychain.RecommendedSeedLen)
	if err != nil {
		log.Error(err)
		return "", err
	}

	return walletseed.EncodeMnemonic(seed), nil
}

func (lw *LibWallet) VerifySeed(seedMnemonic string) bool {
	_, err := walletseed.DecodeUserInput(seedMnemonic)
	return err == nil
}

func (lw *LibWallet) IsNetBackendNil() bool {
	_, err := lw.wallet.NetworkBackend()
	if err != nil {
		log.Error(err)
		return true
	}
	return false
}

func (lw *LibWallet) StartRPCClient(rpcHost string, rpcUser string, rpcPass string, certs []byte) error {
	fmt.Println("Connecting to rpc client")
	ctx := contextWithShutdownCancel(context.Background())
	networkAddress, err := NormalizeAddress(rpcHost, "19109")
	if err != nil {
		log.Error(err)
		return err
	}
	c, err := chain.NewRPCClient(netparams.TestNet3Params.Params, networkAddress,
		rpcUser, rpcPass, certs, false)
	if err != nil {
		log.Error(err)
		return err
	}

	err = c.Start(ctx, false)
	if err != nil {
		log.Error(err)
		return err
	}

	lw.netBackend = chain.BackendFromRPCClient(c.Client)
	lw.wallet.SetNetworkBackend(lw.netBackend)
	lw.loader.SetNetworkBackend(lw.netBackend)
	lw.rpcClient = c
	return nil
}

func (lw *LibWallet) StartSPVConnection(peerAddress string) {
	go func() {
		ctx := contextWithShutdownCancel(context.Background())
		addr := &net.TCPAddr{IP: net.ParseIP("::1"), Port: 19108}
		amgrDir := filepath.Join(lw.dataDir, lw.wallet.ChainParams().Name)
		amgr := addrmgr.New(amgrDir, net.LookupIP) // TODO: be mindful of tor
		lp := p2p.NewLocalPeer(lw.wallet.ChainParams(), addr, amgr)
		syncer := spv.NewSyncer(lw.wallet, lp)
		if len(peerAddress) > 0 {
			//Seperate peer address with a semi-colon ";"
			syncer.SetPersistantPeers(strings.Split(peerAddress, ";"))
		}
		lw.wallet.SetNetworkBackend(syncer)
		lw.loader.SetNetworkBackend(syncer)
		lw.spvSyncer = syncer
		for {
			err := syncer.Run(ctx)
			if done(ctx) {
				log.Info("Syncer Context is done")
				return
			}
			log.Errorf("SPV synchronization ended: %v", err)
		}
	}()
}

func (lw *LibWallet) SpvSync(syncResponse SpvSyncResponse, peerAddresses string, discoverAccounts bool, privatePassphrase []byte) error {
	wallet, ok := lw.loader.LoadedWallet()
	if !ok {
		return errors.E(errors.Invalid, "Wallet has not been loaded")
	}

	if discoverAccounts && len(privatePassphrase) == 0 {
		return errors.E(errors.Invalid, "private passphrase is required for discovering accounts")
	}
	var lockWallet func()
	if discoverAccounts {
		lock := make(chan time.Time, 1)
		lockWallet = func() {
			lock <- time.Time{}
			for i := range privatePassphrase {
				privatePassphrase[i] = 0
			}
		}
		err := wallet.Unlock(privatePassphrase, lock)
		if err != nil {
			return err
		}
	}
	addr := &net.TCPAddr{IP: net.ParseIP("::1"), Port: 0}
	amgrDir := filepath.Join(lw.dataDir, lw.wallet.ChainParams().Name)
	amgr := addrmgr.New(amgrDir, net.LookupIP) // TODO: be mindful of tor
	lp := p2p.NewLocalPeer(wallet.ChainParams(), addr, amgr)

	ntfns := &spv.Notifications{
		Synced: func(sync bool) {
			syncResponse.OnSynced(sync)
			// Lock the wallet after the first time synced while also
			// discovering accounts.
			if sync && lockWallet != nil {
				lockWallet()
				lockWallet = nil
			}
		},
		FetchedHeaders: func(peerInitialHeight, fetchedHeadersCount int32, lastHeaderTime int64) {
			syncResponse.OnFetchedHeaders(peerInitialHeight, fetchedHeadersCount, lastHeaderTime)
		},
		FetchMissingCFilters: func(fetchedCfiltersCount int32) {
			syncResponse.OnFetchMissingCFilters(fetchedCfiltersCount)
		},
		DiscoveredAddresses: func(finished bool) {
			syncResponse.OnDiscoveredAddresses(finished)
		},
		RescanProgress: func(rescannedThrough int32) {
			syncResponse.OnRescanProgress(rescannedThrough)
		},
		PeerDisconnected: func(peerCount int32) {
			syncResponse.OnPeerDisconnected(peerCount)
		},
		PeerConnected: func(peerCount int32) {
			syncResponse.OnPeerConnected(peerCount)
		},
	}
	var spvConnect []string
	if len(peerAddresses) > 0 {
		spvConnect = strings.Split(peerAddresses, ";")
	}
	go func() {
		syncer := spv.NewSyncer(wallet, lp)
		syncer.SetNotifications(ntfns)
		if len(spvConnect) > 0 {
			spvConnects := make([]string, len(spvConnect))
			for i := 0; i < len(spvConnect); i++ {
				spvConnect, err := NormalizeAddress(spvConnect[i], lw.activeNet.Params.DefaultPort)
				if err != nil {
					syncResponse.OnSyncError(3, errors.E("SPV Connect address invalid: %v", err))
					return
				}
				spvConnects[i] = spvConnect
			}
			syncer.SetPersistantPeers(spvConnects)
		}
		wallet.SetNetworkBackend(syncer)
		lw.loader.SetNetworkBackend(syncer)
		ctx := contextWithShutdownCancel(context.Background())
		err := syncer.Run(ctx)
		if err != nil {
			if err == context.Canceled {
				syncResponse.OnSyncError(1, errors.E("SPV synchronization canceled: %v", err))
				return
			} else if err == context.DeadlineExceeded {
				syncResponse.OnSyncError(2, errors.E("SPV synchronization deadline exceeded: %v", err))
				return
			}
			syncResponse.OnSyncError(-1, err)
			return
		}
	}()
	return nil
}

func (lw *LibWallet) RescanPoint() []byte {
	rescanPoint, err := lw.wallet.RescanPoint()
	if err != nil {
		fmt.Println("Couldn't get rescan point:", err)
	}
	if rescanPoint != nil {
		return rescanPoint[:]
	}
	return nil
}

func done(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func (lw *LibWallet) DiscoverActiveAddresses() error {
	wallet, ok := lw.loader.LoadedWallet()
	if !ok {
		return fmt.Errorf("Wallet has not been loaded")
	}

	lw.mu.Lock()
	chainClient := lw.rpcClient
	lw.mu.Unlock()
	if chainClient == nil {
		log.Error("Consensus server RPC client has not been loaded")
		return errors.New("Consensus server RPC client has not been loaded")
	}

	discoverAccounts := !lw.wallet.Locked()

	n := chain.BackendFromRPCClient(chainClient.Client)
	err := wallet.DiscoverActiveAddresses(contextWithShutdownCancel(context.Background()), n, wallet.ChainParams().GenesisHash, discoverAccounts)
	return err
}

func (lw *LibWallet) FetchHeaders() (int32, error) {
	fmt.Println("Fetching Headers")
	count, _, rescanFromHeight, _, _, err := lw.wallet.FetchHeaders(contextWithShutdownCancel(context.Background()), lw.netBackend)
	if err != nil {
		log.Error(err)
		return 0, err
	}
	fmt.Printf("Fetched %v New Headers", count)
	if count > 0 {
		return rescanFromHeight, nil
	}
	return -1, nil
}

func (lw *LibWallet) LoadActiveDataFilters() error {
	fmt.Println("Loading Active Data Filters")
	err := lw.wallet.LoadActiveDataFilters(contextWithShutdownCancel(context.Background()), lw.netBackend, false)
	if err != nil {
		log.Error(err)
	}
	return err
}

func int32ToString(arr []int32) []string {
	var result []string
	for _, i := range arr {
		result = append(result, strconv.Itoa(int(i)))
	}
	return result
}

func (lw *LibWallet) TransactionNotification(listener TransactionListener) {
	go func() {
		n := lw.wallet.NtfnServer.TransactionNotifications()
		defer n.Done()
		for {
			v := <-n.C
			for _, transaction := range v.UnminedTransactions {
				var amount int64
				var inputAmounts int64
				var outputAmounts int64
				tempCredits := make([]TransactionCredit, len(transaction.MyOutputs))
				for index, credit := range transaction.MyOutputs {
					outputAmounts += int64(credit.Amount)
					tempCredits[index] = TransactionCredit{
						Index:    int32(credit.Index),
						Account:  int32(credit.Account),
						Internal: credit.Internal,
						Amount:   int64(credit.Amount),
						Address:  credit.Address.String()}
				}
				tempDebits := make([]TransactionDebit, len(transaction.MyInputs))
				for index, debit := range transaction.MyInputs {
					inputAmounts += int64(debit.PreviousAmount)
					tempDebits[index] = TransactionDebit{
						Index:           int32(debit.Index),
						PreviousAccount: int32(debit.PreviousAccount),
						PreviousAmount:  int64(debit.PreviousAmount),
						AccountName:     lw.GetAccountName(int32(debit.PreviousAccount))}
				}
				var direction int32
				amountDifference := outputAmounts - inputAmounts
				if amountDifference < 0 && (float64(transaction.Fee) == math.Abs(float64(amountDifference))) {
					//Transfered
					direction = 2
					amount = int64(transaction.Fee)
				} else if amountDifference > 0 {
					//Received
					direction = 1
					for _, credit := range transaction.MyOutputs {
						amount += int64(credit.Amount)
					}
				} else {
					//Sent
					direction = 0
					for _, debit := range transaction.MyInputs {
						amount += int64(debit.PreviousAmount)
					}
					for _, credit := range transaction.MyOutputs {
						amount -= int64(credit.Amount)
					}
					amount -= int64(transaction.Fee)
				}
				tempTransaction := Transaction{
					Fee:       int64(transaction.Fee),
					Hash:      fmt.Sprintf("%02x", reverse(transaction.Hash[:])),
					Timestamp: transaction.Timestamp,
					Type:      transactionType(transaction.Type),
					Credits:   &tempCredits,
					Amount:    amount,
					Height:    -1,
					Direction: direction,
					Debits:    &tempDebits}
				fmt.Println("New Transaction")
				result, err := json.Marshal(tempTransaction)
				if err != nil {
					log.Error(err)
				} else {
					listener.OnTransaction(string(result))
				}
			}
			for _, block := range v.AttachedBlocks {
				listener.OnBlockAttached(int32(block.Header.Height))
				for _, transaction := range block.Transactions {
					listener.OnTransactionConfirmed(fmt.Sprintf("%02x", reverse(transaction.Hash[:])), int32(block.Header.Height))
				}
			}
		}
	}()
}

func (lw *LibWallet) SubscribeToBlockNotifications(listener BlockNotificationError) error {
	wallet, ok := lw.loader.LoadedWallet()
	if !ok {
		log.Error("Wallet has not been loaded")
		return errors.New("Wallet has not been loaded")
	}
	if lw.rpcClient == nil {
		log.Error("Consensus server RPC client has not been loaded")
		return errors.New("Consensus server RPC client has not been loaded")
	}

	err := lw.rpcClient.NotifyBlocks()
	if err != nil {
		log.Error(err)
		return err
	}
	wallet.SetNetworkBackend(chain.BackendFromRPCClient(lw.rpcClient.Client))
	go func() {
		syncer := chain.NewRPCSyncer(lw.wallet, lw.rpcClient)
		err = syncer.Run(contextWithShutdownCancel(context.Background()), false)
		log.Infof("Syncer returned")
		if err == context.Canceled {
			fmt.Println("Context was cancelled")
			return
		}
		lw.netBackend = nil
		wallet.SetNetworkBackend(nil)
		listener.OnBlockNotificationError(err)
		log.Error(err)
	}()
	return nil
}

func (lw *LibWallet) OpenWallet() error {

	pubPass := []byte(wallet.InsecurePubPassphrase)
	w, err := lw.loader.OpenExistingWallet(pubPass)
	if err != nil {
		log.Error(err)
		return err
	}
	lw.wallet = w
	return nil
}

func (lw *LibWallet) Rescan(startHeight int32, response BlockScanResponse) {
	go func() {
		if lw.netBackend == nil {
			response.OnError(1, "No network backend")
			return
		}
		if startHeight < 0 {
			response.OnError(2, "Begin height must be non-negative")
			return
		}
		progress := make(chan wallet.RescanProgress, 1)
		ctx := contextWithShutdownCancel(context.Background())
		n, _ := lw.wallet.NetworkBackend()
		var totalHeight int32
		go lw.wallet.RescanProgressFromHeight(ctx, n, startHeight, progress)
		for p := range progress {
			if p.Err != nil {
				log.Error(p.Err)
				response.OnError(-1, p.Err.Error())
				return
			}
			totalHeight += p.ScannedThrough
			if !response.OnScan(p.ScannedThrough) {
				break
			}
		}
		select {
		case <-ctx.Done():
			response.OnEnd(totalHeight, true)
		default:
			response.OnEnd(totalHeight, false)
		}
	}()
}

func (lw *LibWallet) IsAddressMine(address string) bool {
	addr, err := decodeAddress(address, lw.wallet.ChainParams())
	if err != nil {
		log.Error(err)
		return false
	}
	_, err = lw.wallet.AddressInfo(addr)
	return err == nil
}

func (lw *LibWallet) IsAddressValid(address string) bool {
	_, err := decodeAddress(address, lw.wallet.ChainParams())
	if err != nil {
		log.Error(err)
		return false
	}
	return true
}

func (lw *LibWallet) GetAccountName(account int32) string {
	name, err := lw.wallet.AccountName(uint32(account))
	if err != nil {
		log.Error(err)
		return "Account not found"
	}
	return name
}

func (lw *LibWallet) GetAccountByAddress(address string) string {
	addr, err := dcrutil.DecodeAddress(address)
	if err != nil {
		log.Error(err)
		return "Address decode error"
	}
	info, _ := lw.wallet.AddressInfo(addr)
	return lw.GetAccountName(int32(info.Account()))
}

func (lw *LibWallet) GetTransactions(response GetTransactionsResponse) error {
	ctx := contextWithShutdownCancel(context.Background())
	var startBlock, endBlock *wallet.BlockIdentifier
	transactions := make([]Transaction, 0)
	rangeFn := func(block *wallet.Block) (bool, error) {
		for _, transaction := range block.Transactions {
			var inputAmounts int64
			var outputAmounts int64
			var amount int64
			tempCredits := make([]TransactionCredit, len(transaction.MyOutputs))
			for index, credit := range transaction.MyOutputs {
				outputAmounts += int64(credit.Amount)
				tempCredits[index] = TransactionCredit{
					Index:    int32(credit.Index),
					Account:  int32(credit.Account),
					Internal: credit.Internal,
					Amount:   int64(credit.Amount),
					Address:  credit.Address.String()}
			}
			tempDebits := make([]TransactionDebit, len(transaction.MyInputs))
			for index, debit := range transaction.MyInputs {
				inputAmounts += int64(debit.PreviousAmount)
				tempDebits[index] = TransactionDebit{
					Index:           int32(debit.Index),
					PreviousAccount: int32(debit.PreviousAccount),
					PreviousAmount:  int64(debit.PreviousAmount),
					AccountName:     lw.GetAccountName(int32(debit.PreviousAccount))}
			}
			var direction int32
			amountDifference := outputAmounts - inputAmounts
			if amountDifference < 0 && (float64(transaction.Fee) == math.Abs(float64(amountDifference))) {
				//Transfered
				direction = 2
				amount = int64(transaction.Fee)
			} else if amountDifference > 0 {
				//Received
				direction = 1
				for _, credit := range transaction.MyOutputs {
					amount += int64(credit.Amount)
				}
			} else {
				//Sent
				direction = 0
				for _, debit := range transaction.MyInputs {
					amount += int64(debit.PreviousAmount)
				}
				for _, credit := range transaction.MyOutputs {
					amount -= int64(credit.Amount)
				}
				amount -= int64(transaction.Fee)
			}
			var height int32 = -1
			if block.Header != nil {
				height = int32(block.Header.Height)
			}
			tempTransaction := Transaction{
				Fee:       int64(transaction.Fee),
				Hash:      fmt.Sprintf("%02x", reverse(transaction.Hash[:])),
				Timestamp: transaction.Timestamp,
				Type:      transactionType(transaction.Type),
				Credits:   &tempCredits,
				Amount:    amount,
				Height:    height,
				Direction: direction,
				Debits:    &tempDebits}
			transactions = append(transactions, tempTransaction)
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		default:
			return false, nil
		}
	}
	err := lw.wallet.GetTransactions(rangeFn, startBlock, endBlock)
	result, _ := json.Marshal(getTransactionsResponse{ErrorOccurred: false, Transactions: transactions})
	response.OnResult(string(result))
	return err
}

func (lw *LibWallet) DecodeTransaction(txHash []byte) (string, error) {
	hash, err := chainhash.NewHash(txHash)
	if err != nil {
		log.Error(err)
		return "", err
	}
	txSummary, _, _, err := lw.wallet.TransactionSummary(hash)
	if err != nil {
		log.Error(err)
		return "", err
	}
	serializedTx := txSummary.Transaction
	var mtx wire.MsgTx
	err = mtx.Deserialize(bytes.NewReader(serializedTx))
	if err != nil {
		log.Error(err)
		return "", err
	}

	var tx = DecodedTransaction{
		Hash:     fmt.Sprintf("%02x", reverse(hash[:])),
		Type:     transactionType(wallet.TxTransactionType(&mtx)),
		Version:  int32(mtx.Version),
		LockTime: int32(mtx.LockTime),
		Expiry:   int32(mtx.Expiry),
		Inputs:   decodeTxInputs(&mtx),
		Outputs:  decodeTxOutputs(&mtx, lw.chainParams),
	}
	result, _ := json.Marshal(tx)
	return string(result), nil
}

func decodeTxInputs(mtx *wire.MsgTx) []DecodedInput {
	inputs := make([]DecodedInput, len(mtx.TxIn))
	for i, txIn := range mtx.TxIn {

		inputs[i] = DecodedInput{
			PreviousTransactionHash:  fmt.Sprintf("%02x", reverse(txIn.PreviousOutPoint.Hash[:])),
			PreviousTransactionIndex: int32(txIn.PreviousOutPoint.Index),
			Sequence:                 int32(txIn.Sequence),
			AmountIn:                 txIn.ValueIn,
			BlockHeight:              int32(txIn.BlockHeight),
			BlockIndex:               int32(txIn.BlockIndex),
		}
	}
	return inputs
}

func decodeTxOutputs(mtx *wire.MsgTx, chainParams *chaincfg.Params) []DecodedOutput {
	outputs := make([]DecodedOutput, len(mtx.TxOut))
	txType := stake.DetermineTxType(mtx)
	for i, v := range mtx.TxOut {

		var addrs []dcrutil.Address
		var encodedAddrs []string
		if (txType == stake.TxTypeSStx) && (stake.IsStakeSubmissionTxOut(i)) {
			addr, err := stake.AddrFromSStxPkScrCommitment(v.PkScript,
				chainParams)
			if err != nil {
				encodedAddrs = []string{fmt.Sprintf(
					"[error] failed to decode ticket "+
						"commitment addr output for tx hash "+
						"%v, output idx %v", mtx.TxHash(), i)}
			} else {
				encodedAddrs = []string{addr.EncodeAddress()}
			}
		} else {
			// Ignore the error here since an error means the script
			// couldn't parse and there is no additional information
			// about it anyways.
			_, addrs, _, _ = txscript.ExtractPkScriptAddrs(
				v.Version, v.PkScript, chainParams)
			encodedAddrs = make([]string, len(addrs))
			for j, addr := range addrs {
				encodedAddrs[j] = addr.EncodeAddress()
			}
		}

		outputs[i] = DecodedOutput{
			Index:     int32(i),
			Value:     v.Value,
			Version:   int32(v.Version),
			Addresses: encodedAddrs,
		}
	}

	return outputs
}

func reverse(hash []byte) []byte {
	for i := 0; i < len(hash)/2; i++ {
		j := len(hash) - i - 1
		hash[i], hash[j] = hash[j], hash[i]
	}
	return hash
}

func transactionType(txType wallet.TransactionType) string {
	switch txType {
	case wallet.TransactionTypeCoinbase:
		return "COINBASE"
	case wallet.TransactionTypeTicketPurchase:
		return "TICKET_PURCHASE"
	case wallet.TransactionTypeVote:
		return "VOTE"
	case wallet.TransactionTypeRevocation:
		return "REVOCATION"
	default:
		return "REGULAR"
	}
}

func (lw *LibWallet) GetBestBlock() int32 {
	_, height := lw.wallet.MainChainTip()
	return height
}

func (lw *LibWallet) GetBestBlockTimeStamp() int64 {
	_, height := lw.wallet.MainChainTip()
	identifier := wallet.NewBlockIdentifierFromHeight(height)
	info, err := lw.wallet.BlockInfo(identifier)
	if err != nil {
		log.Error(err)
		return 0
	}
	return info.Timestamp
}

func (lw *LibWallet) PublishUnminedTransactions() error {
	if lw.netBackend == nil {
		return errors.New("wallet is not associated with a consensus server RPC client")
	}
	err := lw.wallet.PublishUnminedTransactions(contextWithShutdownCancel(context.Background()), lw.netBackend)
	return err
}

func (lw *LibWallet) SpendableForAccount(account int32, requiredConfirmations int32) (int64, error) {
	bals, err := lw.wallet.CalculateAccountBalance(uint32(account), requiredConfirmations)
	if err != nil {
		log.Error(err)
		return 0, err
	}
	return int64(bals.Spendable), nil
}

func (lw *LibWallet) AddressForAccount(account int32) (string, error) {
	var callOpts []wallet.NextAddressCallOption
	callOpts = append(callOpts, wallet.WithGapPolicyWrap())

	addr, err := lw.wallet.NewExternalAddress(uint32(account), callOpts...)
	if err != nil {
		log.Error(err)
		return "", err
	}
	return addr.EncodeAddress(), nil
}

func (lw *LibWallet) ConstructTransaction(destAddr string, amount int64, srcAccount int32, requiredConfirmations int32, sendAll bool) (*ConstructTxResponse, error) {
	// output destination
	addr, err := dcrutil.DecodeAddress(destAddr)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	version := txscript.DefaultScriptVersion

	// pay output
	outputs := make([]*wire.TxOut, 0)
	var algo wallet.OutputSelectionAlgorithm = wallet.OutputSelectionAlgorithmAll
	if !sendAll {
		algo = wallet.OutputSelectionAlgorithmDefault
		output := &wire.TxOut{
			Value:    amount,
			Version:  version,
			PkScript: pkScript,
		}
		outputs = append(outputs, output)
	}
	feePerKb := txrules.DefaultRelayFeePerKb

	// create tx
	tx, err := lw.wallet.NewUnsignedTransaction(outputs, feePerKb, uint32(srcAccount),
		requiredConfirmations, algo, nil)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	var txBuf bytes.Buffer
	txBuf.Grow(tx.Tx.SerializeSize())
	err = tx.Tx.Serialize(&txBuf)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	var totalOutput dcrutil.Amount
	for _, txOut := range outputs {
		totalOutput += dcrutil.Amount(txOut.Value)
	}
	return &ConstructTxResponse{
		TotalOutputAmount:         int64(totalOutput),
		UnsignedTransaction:       txBuf.Bytes(),
		TotalPreviousOutputAmount: int64(tx.TotalInput),
		EstimatedSignedSize:       int32(tx.EstimatedSignedSerializeSize)}, nil
}

func (lw *LibWallet) RunGC() {
	debug.FreeOSMemory()
}

func (lw *LibWallet) SendTransaction(privPass []byte, destAddr string, amount int64, srcAccount int32, requiredConfs int32, sendAll bool) ([]byte, error) {
	n, err := lw.wallet.NetworkBackend()
	if err != nil {
		log.Error(err)
		return nil, err
	}
	defer func() {
		for i := range privPass {
			privPass[i] = 0
		}
	}()
	// output destination
	addr, err := dcrutil.DecodeAddress(destAddr)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	// pay output
	outputs := make([]*wire.TxOut, 0)
	var algo wallet.OutputSelectionAlgorithm = wallet.OutputSelectionAlgorithmAll
	if !sendAll {
		algo = wallet.OutputSelectionAlgorithmDefault
		output := &wire.TxOut{
			Value:    amount,
			Version:  txscript.DefaultScriptVersion,
			PkScript: pkScript,
		}
		outputs = append(outputs, output)
	}

	// create tx
	unsignedTx, err := lw.wallet.NewUnsignedTransaction(outputs, txrules.DefaultRelayFeePerKb, uint32(srcAccount),
		requiredConfs, algo, nil)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	var txBuf bytes.Buffer
	txBuf.Grow(unsignedTx.Tx.SerializeSize())
	err = unsignedTx.Tx.Serialize(&txBuf)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	var tx wire.MsgTx
	err = tx.Deserialize(bytes.NewReader(txBuf.Bytes()))
	if err != nil {
		log.Error(err)
		//Bytes do not represent a valid raw transaction
		return nil, err
	}

	lock := make(chan time.Time, 1)
	defer func() {
		lock <- time.Time{}
	}()

	err = lw.wallet.Unlock(privPass, lock)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	var additionalPkScripts map[wire.OutPoint][]byte

	invalidSigs, err := lw.wallet.SignTransaction(&tx, txscript.SigHashAll, additionalPkScripts, nil, nil)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	invalidInputIndexes := make([]uint32, len(invalidSigs))
	for i, e := range invalidSigs {
		invalidInputIndexes[i] = e.InputIndex
	}

	var serializedTransaction bytes.Buffer
	serializedTransaction.Grow(tx.SerializeSize())
	err = tx.Serialize(&serializedTransaction)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	var msgTx wire.MsgTx
	err = msgTx.Deserialize(bytes.NewReader(serializedTransaction.Bytes()))
	if err != nil {
		//Invalid tx
		log.Error(err)
		return nil, err
	}

	txHash, err := lw.wallet.PublishTransaction(&msgTx, serializedTransaction.Bytes(), n)

	return txHash[:], err
}

func (lw *LibWallet) GetAccounts(requiredConfirmations int32) (string, error) {
	resp, err := lw.wallet.Accounts()
	if err != nil {
		log.Error("Unable to get accounts from wallet")
		return "", errors.New("Unable to get accounts from wallet")
	}
	accounts := make([]Account, len(resp.Accounts))
	for i := range resp.Accounts {
		a := &resp.Accounts[i]
		bals, err := lw.wallet.CalculateAccountBalance(a.AccountNumber, requiredConfirmations)
		if err != nil {
			log.Errorf("Unable to calculate balance for account %v",
				a.AccountNumber)
			return "", fmt.Errorf("Unable to calculate balance for account %v",
				a.AccountNumber)
		}
		balance := Balance{
			Total:                   int64(bals.Total),
			Spendable:               int64(bals.Spendable),
			ImmatureReward:          int64(bals.ImmatureCoinbaseRewards),
			ImmatureStakeGeneration: int64(bals.ImmatureStakeGeneration),
			LockedByTickets:         int64(bals.LockedByTickets),
			VotingAuthority:         int64(bals.VotingAuthority),
			UnConfirmed:             int64(bals.Unconfirmed),
		}
		accounts[i] = Account{
			Number:           int32(a.AccountNumber),
			Name:             a.AccountName,
			TotalBalance:     int64(a.TotalBalance),
			Balance:          &balance,
			ExternalKeyCount: int32(a.LastUsedExternalIndex + 20),
			InternalKeyCount: int32(a.LastUsedInternalIndex + 20),
			ImportedKeyCount: int32(a.ImportedKeyCount),
		}
	}
	accountsResponse := &Accounts{
		Count:              len(resp.Accounts),
		CurrentBlockHash:   resp.CurrentBlockHash[:],
		CurrentBlockHeight: resp.CurrentBlockHeight,
		Acc:                &accounts,
		ErrorOccurred:      false,
	}
	result, _ := json.Marshal(accountsResponse)
	return string(result), nil
}

func (lw *LibWallet) NextAccount(accountName string, privPass []byte) bool {
	lock := make(chan time.Time, 1)
	defer func() {
		for i := range privPass {
			privPass[i] = 0
		}
		lock <- time.Time{} // send matters, not the value
	}()
	err := lw.wallet.Unlock(privPass, lock)
	if err != nil {
		log.Error(err)
		return false
	}

	_, err = lw.wallet.NextAccount(accountName)
	if err != nil {
		log.Error(err)
		return false
	}
	return true
}

func (lw *LibWallet) RenameAccount(accountNumber int32, newName string) error {
	err := lw.wallet.RenameAccount(uint32(accountNumber), newName)
	return err
}

func (lw *LibWallet) CallJSONRPC(method string, args string, address string, username string, password string, caCert string) (string, error) {
	arguments := strings.Split(args, ",")
	params := make([]interface{}, 0)
	for _, arg := range arguments {
		if strings.TrimSpace(arg) == "" {
			continue
		}
		params = append(params, strings.TrimSpace(arg))
	}
	// Attempt to create the appropriate command using the arguments
	// provided by the user.
	cmd, err := dcrjson.NewCmd(method, params...)
	if err != nil {
		// Show the error along with its error code when it's a
		// dcrjson.Error as it reallistcally will always be since the
		// NewCmd function is only supposed to return errors of that
		// type.
		if jerr, ok := err.(dcrjson.Error); ok {
			log.Errorf("%s command: %v (code: %s)\n",
				method, err, jerr.Code)
			return "", err
		}
		// The error is not a dcrjson.Error and this really should not
		// happen.  Nevertheless, fallback to just showing the error
		// if it should happen due to a bug in the package.
		log.Errorf("%s command: %v\n", method, err)
		return "", err
	}

	// Marshal the command into a JSON-RPC byte slice in preparation for
	// sending it to the RPC server.
	marshalledJSON, err := dcrjson.MarshalCmd("1.0", 1, cmd)
	if err != nil {
		log.Error(err)
		return "", err
	}

	// Send the JSON-RPC request to the server using the user-specified
	// connection configuration.
	result, err := sendPostRequest(marshalledJSON, address, username, password, caCert)
	if err != nil {
		log.Error(err)
		return "", err
	}

	// Choose how to display the result based on its type.
	strResult := string(result)
	if strings.HasPrefix(strResult, "{") || strings.HasPrefix(strResult, "[") {
		var dst bytes.Buffer
		if err := json.Indent(&dst, result, "", "  "); err != nil {
			log.Errorf("Failed to format result: %v", err)
			return "", err
		}
		fmt.Println(dst.String())
		return dst.String(), nil

	} else if strings.HasPrefix(strResult, `"`) {
		var str string
		if err := json.Unmarshal(result, &str); err != nil {
			log.Errorf("Failed to unmarshal result: %v", err)
			return "", err
		}
		fmt.Println(str)
		return str, nil

	} else if strResult != "null" {
		fmt.Println(strResult)
		return strResult, nil
	}
	return "", nil
}
