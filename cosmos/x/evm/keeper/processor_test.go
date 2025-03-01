// SPDX-License-Identifier: BUSL-1.1
//
// Copyright (C) 2023, Berachain Foundation. All rights reserved.
// Use of this software is govered by the Business Source License included
// in the LICENSE file of this repository and at www.mariadb.com/bsl11.
//
// ANY USE OF THE LICENSED WORK IN VIOLATION OF THIS LICENSE WILL AUTOMATICALLY
// TERMINATE YOUR RIGHTS UNDER THIS LICENSE FOR THE CURRENT AND ALL OTHER
// VERSIONS OF THE LICENSED WORK.
//
// THIS LICENSE DOES NOT GRANT YOU ANY RIGHT IN ANY TRADEMARK OR LOGO OF
// LICENSOR OR ITS AFFILIATES (PROVIDED THAT YOU MAY USE A TRADEMARK OR LOGO OF
// LICENSOR AS EXPRESSLY REQUIRED BY THIS LICENSE).
//
// TO THE EXTENT PERMITTED BY APPLICABLE LAW, THE LICENSED WORK IS PROVIDED ON
// AN “AS IS” BASIS. LICENSOR HEREBY DISCLAIMS ALL WARRANTIES AND CONDITIONS,
// EXPRESS OR IMPLIED, INCLUDING (WITHOUT LIMITATION) WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, NON-INFRINGEMENT, AND
// TITLE.

package keeper_test

import (
	"math/big"
	"os"

	storetypes "cosmossdk.io/store/types"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	simtestutil "github.com/cosmos/cosmos-sdk/testutil/sims"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkmempool "github.com/cosmos/cosmos-sdk/types/mempool"
	stakingkeeper "github.com/cosmos/cosmos-sdk/x/staking/keeper"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	pcgenerated "pkg.berachain.dev/polaris/contracts/bindings/cosmos/precompile"
	bindings "pkg.berachain.dev/polaris/contracts/bindings/testing"
	"pkg.berachain.dev/polaris/cosmos/precompile/staking"
	testutil "pkg.berachain.dev/polaris/cosmos/testing/utils"
	"pkg.berachain.dev/polaris/cosmos/x/evm/keeper"
	"pkg.berachain.dev/polaris/cosmos/x/evm/plugins/state"
	evmmempool "pkg.berachain.dev/polaris/cosmos/x/evm/plugins/txpool/mempool"
	"pkg.berachain.dev/polaris/cosmos/x/evm/types"
	"pkg.berachain.dev/polaris/eth/accounts/abi"
	"pkg.berachain.dev/polaris/eth/common"
	"pkg.berachain.dev/polaris/eth/core/precompile"
	coretypes "pkg.berachain.dev/polaris/eth/core/types"
	"pkg.berachain.dev/polaris/eth/core/vm"
	"pkg.berachain.dev/polaris/eth/crypto"
	"pkg.berachain.dev/polaris/eth/params"
	"pkg.berachain.dev/polaris/lib/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func NewValidator(operator sdk.ValAddress, pubKey cryptotypes.PubKey) (stakingtypes.Validator, error) {
	return stakingtypes.NewValidator(operator, pubKey, stakingtypes.Description{})
}

var (
	PKs = simtestutil.CreateTestPubKeys(500)
)

var _ = Describe("Processor", func() {
	var (
		k            *keeper.Keeper
		ak           state.AccountKeeper
		bk           state.BankKeeper
		sk           stakingkeeper.Keeper
		ctx          sdk.Context
		sc           precompile.StatefulImpl
		key, _       = crypto.GenerateEthKey()
		signer       = coretypes.LatestSignerForChainID(params.DefaultChainConfig.ChainID)
		legacyTxData *coretypes.LegacyTx
		valAddr      = []byte{0x21}
	)

	BeforeEach(func() {
		err := os.RemoveAll("tmp/berachain")
		Expect(err).ToNot(HaveOccurred())

		legacyTxData = &coretypes.LegacyTx{
			Nonce:    0,
			Gas:      10000000,
			Data:     []byte("abcdef"),
			GasPrice: big.NewInt(1),
		}

		// before chain, init genesis state
		ctx, ak, bk, sk = testutil.SetupMinimalKeepers()
		k = keeper.NewKeeper(
			storetypes.NewKVStoreKey("evm"),
			ak, bk,
			func() func() []vm.RegistrablePrecompile { return nil },
			"authority",
			simtestutil.NewAppOptionsWithFlagHome("tmp/berachain"),
			evmmempool.NewEthTxPoolFrom(sdkmempool.NewPriorityMempool()),
		)
		validator, err := NewValidator(sdk.ValAddress(valAddr), PKs[0])
		Expect(err).ToNot(HaveOccurred())
		validator.Status = stakingtypes.Bonded
		sk.SetValidator(ctx, validator)
		sc = staking.NewPrecompileContract(&sk)
		k.Setup(ak, bk, []vm.RegistrablePrecompile{sc}, nil)
		k.ConfigureGethLogger(ctx)
		_ = sk.SetParams(ctx, stakingtypes.DefaultParams())
		for _, plugin := range k.GetAllPlugins() {
			plugin.InitGenesis(ctx, types.DefaultGenesis())
		}

		// before every block
		ctx = ctx.WithBlockGasMeter(storetypes.NewGasMeter(100000000000000)).
			WithKVGasConfig(storetypes.GasConfig{}).
			WithBlockHeight(1)
		k.BeginBlocker(ctx)
	})

	Context("New Block", func() {
		BeforeEach(func() {
			// before every tx
			ctx = ctx.WithGasMeter(storetypes.NewInfiniteGasMeter())
		})

		AfterEach(func() {
			k.EndBlocker(ctx)
			err := os.RemoveAll("tmp/berachain")
			Expect(err).ToNot(HaveOccurred())
		})

		It("should call precompile correctly", func() {
			var contractAbi abi.ABI
			err := contractAbi.UnmarshalJSON([]byte(pcgenerated.StakingModuleMetaData.ABI))
			Expect(err).ToNot(HaveOccurred())

			legacyTxData.Data, err = contractAbi.Pack("getActiveValidators")
			Expect(err).ToNot(HaveOccurred())
			contractAddr := sc.RegistryKey()
			legacyTxData.To = &contractAddr
			tx := coretypes.MustSignNewTx(key, signer, legacyTxData)

			addr, err := signer.Sender(tx)
			Expect(err).ToNot(HaveOccurred())
			k.GetStatePlugin().CreateAccount(addr)
			k.GetStatePlugin().AddBalance(addr, big.NewInt(1000000000))
			k.GetStatePlugin().Finalize()

			vals := sk.GetAllValidators(ctx)
			Expect(vals).To(HaveLen(1))

			// calls the staking precompile
			exec, err := k.ProcessTransaction(ctx, tx)
			Expect(err).ToNot(HaveOccurred())
			ret, err := contractAbi.Methods["getActiveValidators"].Outputs.Unpack(exec.ReturnData)
			Expect(err).ToNot(HaveOccurred())
			addrs, ok := utils.GetAs[[]common.Address](ret[0])
			Expect(ok).To(BeTrue())
			Expect(addrs[0]).To(Equal(common.BytesToAddress(valAddr)))
			Expect(exec.Err).ToNot(HaveOccurred())

			// call the staking precompile again, but this time with a different method
			legacyTxData.Nonce++
			legacyTxData.Data, err = contractAbi.Pack(
				"delegate", common.BytesToAddress(valAddr), big.NewInt(10000000),
			)
			Expect(err).ToNot(HaveOccurred())
			tx = coretypes.MustSignNewTx(key, signer, legacyTxData)
			addr, err = signer.Sender(tx)
			Expect(err).ToNot(HaveOccurred())
			k.GetStatePlugin().AddBalance(addr, big.NewInt(1000000000))
			k.GetStatePlugin().Finalize()
			exec, err = k.ProcessTransaction(ctx, tx)
			Expect(err).ToNot(HaveOccurred())
			ret, err = contractAbi.Methods["delegate"].Outputs.Unpack(exec.ReturnData)
			Expect(err).ToNot(HaveOccurred())
			Expect(ret).To(BeEmpty())
		})

		It("should panic on nil, empty transaction", func() {
			Expect(func() {
				_, err := k.ProcessTransaction(ctx, nil)
				Expect(err).To(HaveOccurred())
			}).To(Panic())
			Expect(func() {
				_, err := k.ProcessTransaction(ctx, &coretypes.Transaction{})
				Expect(err).To(HaveOccurred())
			}).To(Panic())
		})

		It("should successfully deploy a valid contract and call it", func() {
			legacyTxData.Data = common.FromHex(bindings.SolmateERC20Bin)
			tx := coretypes.MustSignNewTx(key, signer, legacyTxData)
			addr, err := signer.Sender(tx)
			Expect(err).ToNot(HaveOccurred())
			k.GetStatePlugin().CreateAccount(addr)
			k.GetStatePlugin().AddBalance(addr, big.NewInt(1000000000))
			k.GetStatePlugin().Finalize()

			// create the contract
			result, err := k.ProcessTransaction(ctx, tx)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Err).ToNot(HaveOccurred())
			// call the contract non-view function
			deployAddress := crypto.CreateAddress(crypto.PubkeyToAddress(key.PublicKey), 0)
			legacyTxData.To = &deployAddress
			var solmateABI abi.ABI
			err = solmateABI.UnmarshalJSON([]byte(bindings.SolmateERC20ABI))
			Expect(err).ToNot(HaveOccurred())
			input, err := solmateABI.Pack("mint", common.BytesToAddress([]byte{0x88}), big.NewInt(8888888))
			Expect(err).ToNot(HaveOccurred())
			legacyTxData.Data = input
			legacyTxData.Nonce++
			tx = coretypes.MustSignNewTx(key, signer, legacyTxData)
			result, err = k.ProcessTransaction(ctx, tx)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Err).ToNot(HaveOccurred())

			// call the contract view function
			legacyTxData.Data = crypto.Keccak256Hash([]byte("totalSupply()")).Bytes()[:4]
			legacyTxData.Nonce++
			tx = coretypes.MustSignNewTx(key, signer, legacyTxData)
			result, err = k.ProcessTransaction(ctx, tx)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Err).ToNot(HaveOccurred())
		})
	})
})
