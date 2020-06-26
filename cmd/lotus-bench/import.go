package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"time"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/stmgr"
	"github.com/filecoin-project/lotus/chain/store"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/vm"
	_ "github.com/filecoin-project/lotus/lib/sigs/bls"
	_ "github.com/filecoin-project/lotus/lib/sigs/secp"
	"github.com/filecoin-project/sector-storage/ffiwrapper"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"golang.org/x/xerrors"

	"github.com/ipfs/go-datastore"
	badger "github.com/ipfs/go-ds-badger2"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/urfave/cli/v2"
)

type TipSetExec struct {
	TipSet   types.TipSetKey
	Trace    []*api.InvocResult
	Duration time.Duration
}

var importBenchCmd = &cli.Command{
	Name:  "import",
	Usage: "benchmark chain import and validation",
	Subcommands: []*cli.Command{
		importAnalyzeCmd,
	},
	Flags: []cli.Flag{
		&cli.Int64Flag{
			Name:  "height",
			Usage: "halt validation after given height",
		},
		&cli.IntFlag{
			Name:  "batch-seal-verify-threads",
			Usage: "set the parallelism factor for batch seal verification",
			Value: runtime.NumCPU(),
		},
	},
	Action: func(cctx *cli.Context) error {
		vm.BatchSealVerifyParallelism = cctx.Int("batch-seal-verify-threads")
		if !cctx.Args().Present() {
			fmt.Println("must pass car file of chain to benchmark importing")
			return nil
		}

		cfi, err := os.Open(cctx.Args().First())
		if err != nil {
			return err
		}
		defer cfi.Close() //nolint:errcheck // read only file

		tdir, err := ioutil.TempDir("", "lotus-import-bench")
		if err != nil {
			return err
		}

		bds, err := badger.NewDatastore(tdir, nil)
		if err != nil {
			return err
		}
		bs := blockstore.NewBlockstore(bds)
		cbs, err := blockstore.CachedBlockstore(context.TODO(), bs, blockstore.DefaultCacheOpts())
		if err != nil {
			return err
		}
		bs = cbs
		ds := datastore.NewMapDatastore()
		cs := store.NewChainStore(bs, ds, vm.Syscalls(ffiwrapper.ProofVerifier))
		stm := stmgr.NewStateManager(cs)

		prof, err := os.Create("import-bench.prof")
		if err != nil {
			return err
		}
		defer prof.Close() //nolint:errcheck

		if err := pprof.StartCPUProfile(prof); err != nil {
			return err
		}

		head, err := cs.Import(cfi)
		if err != nil {
			return err
		}

		if h := cctx.Int64("height"); h != 0 {
			tsh, err := cs.GetTipsetByHeight(context.TODO(), abi.ChainEpoch(h), head, true)
			if err != nil {
				return err
			}
			head = tsh
		}

		ts := head
		tschain := []*types.TipSet{ts}
		for ts.Height() != 0 {
			next, err := cs.LoadTipSet(ts.Parents())
			if err != nil {
				return err
			}

			tschain = append(tschain, next)
			ts = next
		}

		ibj, err := os.Create("import-bench.json")
		if err != nil {
			return err
		}
		defer ibj.Close() //nolint:errcheck

		enc := json.NewEncoder(ibj)

		var lastTse *TipSetExec

		lastState := tschain[len(tschain)-1].ParentState()
		for i := len(tschain) - 2; i >= 0; i-- {
			cur := tschain[i]
			log.Infof("computing state (height: %d, ts=%s)", cur.Height(), cur.Cids())
			if cur.ParentState() != lastState {
				lastTrace := lastTse.Trace
				d, err := json.MarshalIndent(lastTrace, "", "  ")
				if err != nil {
					panic(err)
				}
				fmt.Println("TRACE")
				fmt.Println(string(d))
				return xerrors.Errorf("tipset chain had state mismatch at height %d (%s != %s)", cur.Height(), cur.ParentState(), lastState)
			}
			start := time.Now()
			st, trace, err := stm.ExecutionTrace(context.TODO(), cur)
			if err != nil {
				return err
			}
			stripCallers(trace)

			lastTse = &TipSetExec{
				TipSet:   cur.Key(),
				Trace:    trace,
				Duration: time.Since(start),
			}
			lastState = st
			if err := enc.Encode(lastTse); err != nil {
				return xerrors.Errorf("failed to write out tipsetexec: %w", err)
			}
		}

		pprof.StopCPUProfile()

		return nil

	},
}

func walkExecutionTrace(et *types.ExecutionTrace) {
	for _, gc := range et.GasCharges {
		gc.Callers = nil
	}
	for _, sub := range et.Subcalls {
		walkExecutionTrace(&sub) //nolint:scopelint,gosec
	}
}

func stripCallers(trace []*api.InvocResult) {
	for _, t := range trace {
		walkExecutionTrace(&t.ExecutionTrace)
	}
}

type Invocation struct {
	TipSet types.TipSetKey
	Invoc  *api.InvocResult
}

const GasPerNs = 10

func countGasCosts(et *types.ExecutionTrace) (int64, int64) {
	var cgas, vgas int64

	for _, gc := range et.GasCharges {
		cgas += gc.ComputeGas
		vgas += gc.VirtualComputeGas
	}

	for _, sub := range et.Subcalls {
		c, v := countGasCosts(&sub)
		cgas += c
		vgas += v
	}

	return cgas, vgas
}

func compStats(vals []float64) (float64, float64) {
	var sum float64

	for _, v := range vals {
		sum += v
	}

	av := sum / float64(len(vals))

	var varsum float64
	for _, v := range vals {
		delta := av - v
		varsum += delta * delta
	}

	return av, math.Sqrt(varsum / float64(len(vals)))
}

func tallyGasCharges(charges map[string][]float64, et *types.ExecutionTrace) {
	for _, gc := range et.GasCharges {

		compGas := gc.ComputeGas + gc.VirtualComputeGas
		ratio := float64(compGas) / float64(gc.TimeTaken.Nanoseconds())

		charges[gc.Name] = append(charges[gc.Name], 1/(ratio/GasPerNs))
		//fmt.Printf("%s: %d, %s: %0.2f\n", gc.Name, compGas, gc.TimeTaken, 1/(ratio/GasPerNs))
		for _, sub := range et.Subcalls {
			tallyGasCharges(charges, &sub)
		}
	}

}

var importAnalyzeCmd = &cli.Command{
	Name: "analyze",
	Action: func(cctx *cli.Context) error {
		if !cctx.Args().Present() {
			fmt.Println("must pass bench file to analyze")
			return nil
		}

		fi, err := os.Open(cctx.Args().First())
		if err != nil {
			return err
		}
		dec := json.NewDecoder(fi)

		const nWorkers = 16
		tseIn := make(chan TipSetExec, 2*nWorkers)
		type result struct {
			totalTime       time.Duration
			chargeDeltas    map[string][]float64
			expensiveInvocs []Invocation
		}

		results := make(chan result, nWorkers)

		for i := 0; i < nWorkers; i++ {
			go func() {
				chargeDeltas := make(map[string][]float64)
				var totalTime time.Duration
				var expensiveInvocs []Invocation
				var leastExpensiveInvoc = time.Duration(0)

				for {
					tse, ok := <-tseIn
					if !ok {
						results <- result{
							totalTime:       totalTime,
							chargeDeltas:    chargeDeltas,
							expensiveInvocs: expensiveInvocs,
						}
						return
					}
					totalTime += tse.Duration
					for _, inv := range tse.Trace {
						if inv.Duration > leastExpensiveInvoc {
							expensiveInvocs = append(expensiveInvocs, Invocation{
								TipSet: tse.TipSet,
								Invoc:  inv,
							})
						}

						tallyGasCharges(chargeDeltas, &inv.ExecutionTrace)
					}
					sort.Slice(expensiveInvocs, func(i, j int) bool {
						return expensiveInvocs[i].Invoc.Duration > expensiveInvocs[j].Invoc.Duration
					})
					if len(expensiveInvocs) != 0 {
						leastExpensiveInvoc = expensiveInvocs[len(expensiveInvocs)-1].Invoc.Duration
					}
				}
			}()
		}

		var totalTipsets int64
		for {
			var tse TipSetExec
			if err := dec.Decode(&tse); err != nil {
				if err != io.EOF {
					if e, ok := err.(*json.SyntaxError); ok {
						log.Warnf("syntax error at byte offset %d", e.Offset)
					}
					return err
				}
				break
			}
			totalTipsets++
			tseIn <- tse
			if totalTipsets%10 == 0 {
				fmt.Printf("\rProcessed %d tipsets", totalTipsets)
			}
		}
		close(tseIn)
		fmt.Printf("\n")
		fmt.Printf("Collecting results\n")

		var invocs []Invocation
		var totalTime time.Duration
		var keys []string
		var chargeDeltas = make(map[string][]float64)
		for i := 0; i < nWorkers; i++ {
			fmt.Printf("\rProcessing results from worker %d/%d", i+1, nWorkers)
			res := <-results
			invocs = append(invocs, res.expensiveInvocs...)
			for k, v := range res.chargeDeltas {
				chargeDeltas[k] = append(chargeDeltas[k], v...)
			}
			totalTime += res.totalTime
		}

		fmt.Printf("\nCollecting gas keys\n")
		for k := range chargeDeltas {
			keys = append(keys, k)
		}

		fmt.Println("Gas Price Deltas")
		sort.Strings(keys)
		for _, k := range keys {
			vals := chargeDeltas[k]
			av, stdev := compStats(vals)

			fmt.Printf("%s: incr by %f (%f)\n", k, av, stdev)
		}

		sort.Slice(invocs, func(i, j int) bool {
			return invocs[i].Invoc.Duration > invocs[j].Invoc.Duration
		})

		fmt.Println("Total time: ", totalTime)
		fmt.Println("Average time per epoch: ", totalTime/time.Duration(totalTipsets))

		n := 30
		if len(invocs) < n {
			n = len(invocs)
		}
		fmt.Printf("Top %d most expensive calls:\n", n)
		for i := 0; i < n; i++ {
			inv := invocs[i].Invoc
			fmt.Printf("%s: %s %s %d %s\n", inv.Duration, inv.Msg.From, inv.Msg.To, inv.Msg.Method, invocs[i].TipSet)
		}
		return nil
	},
}
