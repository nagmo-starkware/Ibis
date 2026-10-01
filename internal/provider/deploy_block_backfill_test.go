package provider

import (
	"context"
	"testing"
	"time"
)

// A factory child emits events in its deploy block before the factory's
// DeploymentCreated gets it registered, so a shared stream drops them (the
// address is not tracked yet). The deploy block is often still pre_confirmed
// at registration, putting tip+1 == StartBlock. Registration must re-fetch
// that block anyway, once it is accepted, and stay pending until then.
func TestRegistrationRefetchesUnacceptedDeployBlock(t *testing.T) {
	const tip, deploy, child = 200, 201, 0xC
	for _, site := range gapSites() {
		if site.name == "keys-address-sub" {
			continue // its keys-sub tracks nothing to order the drop against
		}
		shared := site.name != "per-contract"
		t.Run(site.name, func(t *testing.T) {
			chain := newGapChain(tip, tip)
			chain.add(deploy, child)
			node := newGapNode()
			sub, events := startGapSite(t, site, chain, node)
			sess := node.nextFor(t, site)
			waitLive(t, sub)
			chain.pre.Store(deploy) // the deploy block is being built

			isChild := func(e RawEvent) bool {
				return e.ContractAddress != nil && e.ContractAddress.String() == newTestFelt(child).String()
			}
			if shared {
				// Streamed before registration: dropped. The factory's event
				// after it proves the drop happened first.
				sess.events <- liveEvent(child, deploy)
				sess.events <- liveEvent(site.addr, deploy)
				waitEvent(t, events, "factory event", func(e RawEvent) bool {
					if isChild(e) {
						t.Fatal("untracked child event forwarded")
					}
					return e.BlockNumber == deploy
				})
			}

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			sub.AddContract(ctx, ContractSubscription{Address: newTestFelt(child), StartBlock: deploy, Wildcard: true})

			time.Sleep(50 * time.Millisecond) // deploy block still unaccepted
			if _, _, complete := sub.TransportStatus(); complete {
				t.Error("catchup complete while the child's deploy block is unfetched")
			}

			chain.tip.Store(deploy)
			got := waitEvent(t, events, "child deploy-block event", func(e RawEvent) bool { return isChild(e) && !e.BackfillDone })
			if got.BlockNumber != deploy {
				t.Errorf("child event at block %d, want %d", got.BlockNumber, deploy)
			}
			if shared {
				waitEvent(t, events, "child BackfillDone", func(e RawEvent) bool { return isChild(e) && e.BackfillDone })
			}

			deadline := time.After(5 * time.Second)
			for {
				if _, _, complete := sub.TransportStatus(); complete && sub.BackfillsPending() == 0 {
					break
				}
				select {
				case <-deadline:
					t.Fatalf("never complete: pending=%d", sub.BackfillsPending())
				case <-time.After(2 * time.Millisecond):
				}
			}
			if n := chain.ahead.Load(); n != 0 {
				t.Errorf("%d getEvents calls bounded past the accepted tip: must wait, not spin", n)
			}
		})
	}
}
