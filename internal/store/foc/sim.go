//go:build sim

package foc

import (
	"context"
	"math/big"
)

// SimFunding is a Funding for the simulated store that the website's
// recordings use (cmd/ipfs2fil/sim.go); apply stands in for the deposit.
func SimFunding(deposit, monthly, fees, wallet *big.Int, apply func(context.Context) error) *Funding {
	return &Funding{Deposit: deposit, Monthly: monthly, Fees: fees, Wallet: wallet, apply: apply}
}
