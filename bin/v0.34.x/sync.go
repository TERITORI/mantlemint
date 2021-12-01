package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/simapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/gorilla/mux"
	"github.com/spf13/viper"
	tmlog "github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/proxy"
	tendermint "github.com/tendermint/tendermint/types"
	terra "github.com/terra-money/core/app"
	core "github.com/terra-money/core/types"
	wasmconfig "github.com/terra-money/core/x/wasm/config"
	blockFeeder "github.com/terra-money/mantlemint-provider-v0.34.x/block_feed"
	"github.com/terra-money/mantlemint-provider-v0.34.x/config"
	"github.com/terra-money/mantlemint-provider-v0.34.x/db/heleveldb"
	"github.com/terra-money/mantlemint-provider-v0.34.x/db/hld"
	"github.com/terra-money/mantlemint-provider-v0.34.x/db/safe_batch"
	"github.com/terra-money/mantlemint-provider-v0.34.x/indexer"
	"github.com/terra-money/mantlemint-provider-v0.34.x/indexer/block"
	"github.com/terra-money/mantlemint-provider-v0.34.x/indexer/tx"
	"github.com/terra-money/mantlemint-provider-v0.34.x/mantlemint"
	"github.com/terra-money/mantlemint-provider-v0.34.x/rpc"
	"github.com/terra-money/mantlemint-provider-v0.34.x/store/rootmulti"
)

// initialize mantlemint for v0.34.x
func main() {
	mantlemintConfig := config.NewConfig()
	mantlemintConfig.Print()

	viper.SetConfigType("toml")
	viper.SetConfigName("app")
	viper.AddConfigPath(filepath.Join(mantlemintConfig.Home, "config"))

	if err := viper.MergeInConfig(); err != nil {
		panic(fmt.Errorf("failed to merge configuration: %w", err))
	}

	sdkConfig := sdk.GetConfig()
	sdkConfig.SetCoinType(core.CoinType)
	sdkConfig.SetFullFundraiserPath(core.FullFundraiserPath)
	sdkConfig.SetBech32PrefixForAccount(core.Bech32PrefixAccAddr, core.Bech32PrefixAccPub)
	sdkConfig.SetBech32PrefixForValidator(core.Bech32PrefixValAddr, core.Bech32PrefixValPub)
	sdkConfig.SetBech32PrefixForConsensusNode(core.Bech32PrefixConsAddr, core.Bech32PrefixConsPub)
	sdkConfig.SetAddressVerifier(core.AddressVerifier)
	sdkConfig.Seal()

	ldb, ldbErr := heleveldb.NewLevelDBDriver(&heleveldb.DriverConfig{mantlemintConfig.MantlemintDB, mantlemintConfig.Home, heleveldb.DriverModeKeySuffixDesc})
	if ldbErr != nil {
		panic(ldbErr)
	}

	var hldb = hld.ApplyHeightLimitedDB(
		ldb,
		&hld.HeightLimitedDBConfig{
			Debug: true,
		},
	)

	batched := safe_batch.NewSafeBatchDB(hldb)
	batchedOrigin := batched.(safe_batch.SafeBatchDBCloser)
	logger := tmlog.NewTMLogger(os.Stdout)
	codec := terra.MakeEncodingConfig()

	// customize CMS to limit kv store's read height on query
	cms := rootmulti.NewStore(batched, hldb)

	var app = terra.NewTerraApp(
		logger,
		batched,
		nil,
		true, // need this so KVStores are set
		make(map[int64]bool),
		mantlemintConfig.Home,
		0,
		codec,
		simapp.EmptyAppOptions{},
		&wasmconfig.Config{
			ContractQueryGasLimit:   3000000,
			ContractDebugMode:       false,
			ContractMemoryCacheSize: 2048,
		},
		fauxMerkleModeOpt,
		func(ba *baseapp.BaseApp) {
			ba.SetCMS(cms)
		},
	)

	// create app...
	var appCreator = mantlemint.NewConcurrentQueryClientCreator(app)
	appConns := proxy.NewAppConns(appCreator)
	appConns.SetLogger(logger)
	if startErr := appConns.OnStart(); startErr != nil {
		panic(startErr)
	}

	go func() {
		a := <-appConns.Quit()
		fmt.Println(a)
	}()

	var executor = mantlemint.NewMantlemintExecutor(batched, appConns.Consensus())

	var mm = mantlemint.NewMantlemint(
		batched,
		appConns,
		executor,

		// run before
		nil,

		// RunAfter Inject callback
		nil,
	)

	// initialize using provided genesis
	genesisDoc := getGenesisDoc(mantlemintConfig.GenesisPath)
	initialHeight := genesisDoc.InitialHeight
	hldb.SetWriteHeight(initialHeight)
	batchedOrigin.Open()

	if initErr := mm.Init(genesisDoc); initErr != nil {
		panic(initErr)
	}

	if flushErr := batchedOrigin.Flush(); flushErr != nil {
		debug.PrintStack()
		panic(flushErr)
	}

	if loadErr := mm.LoadInitialState(); loadErr != nil {
		panic(loadErr)
	}

	hldb.ClearWriteHeight()

	// get blocks over some sort of transport, inject to mantlemint
	blockFeed := blockFeeder.NewAggregateBlockFeed(
		mm.GetCurrentHeight(),
		mantlemintConfig.RPCEndpoints,
		mantlemintConfig.WSEndpoints,
	)

	// create indexer service
	indexerInstance, indexerInstanceErr := indexer.NewIndexer("indexer", mantlemintConfig.Home)
	if indexerInstanceErr != nil {
		panic(indexerInstanceErr)
	}
	indexerInstance.RegisterIndexerService("tx", tx.IndexTx)
	indexerInstance.RegisterIndexerService("block", block.IndexBlock)

	abcicli, _ := appCreator.NewABCIClient()
	rpccli := rpc.NewRpcClient(abcicli)

	// rest cache invalidate channel
	cacheInvalidateChan := make(chan int64)

	// start RPC server
	rpcErr := rpc.StartRPC(
		app,
		rpccli,
		mantlemintConfig.ChainID,
		codec,
		cacheInvalidateChan,

		// register custom routers; primarily for indexers
		func(router *mux.Router) {
			// create new post router. It would panic on error
			go indexerInstance.
				WithSideSyncRouter(func(sidesyncRouter *mux.Router) {
					indexerInstance.RegisterRESTRoute(router, sidesyncRouter, tx.RegisterRESTRoute)
					indexerInstance.RegisterRESTRoute(router, sidesyncRouter, block.RegisterRESTRoute)
				}).
				StartSideSync(mantlemintConfig.IndexerSideSyncPort)
		},
		// inject flag checker for synced
		blockFeed.IsSynced,
	)

	if rpcErr != nil {
		panic(rpcErr)
	}

	// start subscribing to block
	if mantlemintConfig.DisableSync {
		fmt.Println("running without sync...")
		forever()
	} else {
		if cBlockFeed, blockFeedErr := blockFeed.Subscribe(0); blockFeedErr != nil {
			panic(blockFeedErr)
		} else {
			for {
				feed := <-cBlockFeed

				// open db batch
				hldb.SetWriteHeight(feed.Block.Height)
				batchedOrigin.Open()
				if injectErr := mm.Inject(feed.Block); injectErr != nil {
					debug.PrintStack()
					panic(injectErr)
				}

				// flush db batch
				if flushErr := batchedOrigin.Flush(); flushErr != nil {
					debug.PrintStack()
					panic(flushErr)
				}

				hldb.ClearWriteHeight()

				// run indexer
				if indexerErr := indexerInstance.Run(feed.Block, feed.BlockID, mm.GetCurrentEventCollector()); indexerErr != nil {
					debug.PrintStack()
					panic(indexerErr)
				}

				cacheInvalidateChan <- feed.Block.Height
			}
		}
	}

}

// Pass this in as an option to use a dbStoreAdapter instead of an IAVLStore for simulation speed.
func fauxMerkleModeOpt(app *baseapp.BaseApp) {
	app.SetFauxMerkleMode()
}

func getGenesisDoc(genesisPath string) *tendermint.GenesisDoc {
	jsonBlob, _ := ioutil.ReadFile(genesisPath)
	shasum := sha1.New()
	shasum.Write(jsonBlob)
	sum := hex.EncodeToString(shasum.Sum(nil))

	log.Printf("[v0.34.x/sync] genesis shasum=%s", sum)

	if genesis, genesisErr := tendermint.GenesisDocFromFile(genesisPath); genesisErr != nil {
		panic(genesisErr)
	} else {
		return genesis
	}
}

func forever() {
	<-(chan int)(nil)
}
