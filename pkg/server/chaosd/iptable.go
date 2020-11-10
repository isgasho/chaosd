// Copyright 2020 Chaos Mesh Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package chaosd

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"

	"github.com/chaos-mesh/chaos-daemon/pkg/bpm"
	pb "github.com/chaos-mesh/chaos-daemon/pkg/server/serverpb"
)

const (
	iptablesCmd = "iptables"

	iptablesChainAlreadyExistErr = "iptables: Chain already exists."
)

func (s *Server) SetContainerIptablesChains(ctx context.Context, req *pb.IptablesChainsRequest) error {
	pid, err := s.criCli.GetPidFromContainerID(ctx, req.ContainerId)
	if err != nil {
		log.Error("failed to get pid", zap.Error(err))
		return errors.WithStack(err)
	}

	nsPath := GetNsPath(pid, bpm.NetNS)

	iptables := buildIptablesClient(ctx, nsPath)
	err = iptables.initializeEnv()
	if err != nil {
		log.Error("failed to initialize iptables", zap.Error(err))
		return errors.WithStack(err)
	}

	err = iptables.setIptablesChains(req.Chains)
	if err != nil {
		log.Error("failed to set iptables chains", zap.Error(err))
		return errors.WithStack(err)
	}

	return nil
}

type iptablesClient struct {
	ctx    context.Context
	nsPath string
}

type iptablesChain struct {
	Name  string
	Rules []string
}

func buildIptablesClient(ctx context.Context, nsPath string) *iptablesClient {
	return &iptablesClient{
		ctx,
		nsPath,
	}
}

func (iptables *iptablesClient) setIptablesChains(chains []*pb.Chain) error {
	for _, chain := range chains {
		err := iptables.setIptablesChain(chain)
		if err != nil {
			return err
		}
	}

	return nil
}

func (iptables *iptablesClient) setIptablesChain(chain *pb.Chain) error {
	var matchPart string
	if chain.Direction == pb.Chain_INPUT {
		matchPart = "src"
	} else if chain.Direction == pb.Chain_OUTPUT {
		matchPart = "dst"
	} else {
		return fmt.Errorf("unknown chain direction %d", chain.Direction)
	}

	protocolAndPort := chain.Protocol
	if len(protocolAndPort) > 0 {
		if len(chain.SourcePorts) > 0 {
			protocolAndPort += " " + chain.SourcePorts
		}

		if len(chain.DestinationPorts) > 0 {
			protocolAndPort += " " + chain.DestinationPorts
		}
	}

	rules := []string{}
	for _, ipset := range chain.Ipsets {
		rules = append(rules, fmt.Sprintf("-A %s -m set --match-set %s %s -j %s -w 5 %s",
			chain.Name, ipset, matchPart, chain.Target, protocolAndPort))
	}
	err := iptables.createNewChain(&iptablesChain{
		Name:  chain.Name,
		Rules: rules,
	})
	if err != nil {
		return errors.WithStack(err)
	}

	if chain.Direction == pb.Chain_INPUT {
		err := iptables.ensureRule(&iptablesChain{
			Name: "CHAOS-INPUT",
		}, "-A CHAOS-INPUT -j "+chain.Name)
		if err != nil {
			return errors.WithStack(err)
		}
	} else if chain.Direction == pb.Chain_OUTPUT {
		iptables.ensureRule(&iptablesChain{
			Name: "CHAOS-OUTPUT",
		}, "-A CHAOS-OUTPUT -j "+chain.Name)
		if err != nil {
			return errors.WithStack(err)
		}
	} else {
		return fmt.Errorf("unknown direction %d", chain.Direction)
	}
	return nil
}

func (iptables *iptablesClient) initializeEnv() error {
	for _, direction := range []string{"INPUT", "OUTPUT"} {
		chainName := "CHAOS-" + direction

		err := iptables.createNewChain(&iptablesChain{
			Name:  chainName,
			Rules: []string{},
		})
		if err != nil {
			return err
		}

		iptables.ensureRule(&iptablesChain{
			Name:  direction,
			Rules: []string{},
		}, "-A "+direction+" -j "+chainName)
	}

	return nil
}

// createNewChain will cover existing chain
func (iptables *iptablesClient) createNewChain(chain *iptablesChain) error {
	cmd := bpm.DefaultProcessBuilder(iptablesCmd, "-w", "-N", chain.Name).SetNetNS(iptables.nsPath).SetContext(iptables.ctx).Build()
	out, err := cmd.CombinedOutput()

	if (err == nil && len(out) == 0) ||
		(err != nil && strings.Contains(string(out), iptablesChainAlreadyExistErr)) {
		// Successfully create a new chain
		return iptables.deleteAndWriteRules(chain)
	}

	return encodeOutputToError(out, err)
}

// deleteAndWriteRules will remove all existing function in the chain
// and replace with the new settings
func (iptables *iptablesClient) deleteAndWriteRules(chain *iptablesChain) error {

	// This chain should already exist
	err := iptables.flushIptablesChain(chain)
	if err != nil {
		return err
	}

	for _, rule := range chain.Rules {
		err := iptables.ensureRule(chain, rule)
		if err != nil {
			return err
		}
	}

	return nil
}

func (iptables *iptablesClient) ensureRule(chain *iptablesChain, rule string) error {
	cmd := bpm.DefaultProcessBuilder(iptablesCmd, "-w", "-S", chain.Name).SetNetNS(iptables.nsPath).SetContext(iptables.ctx).Build()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return encodeOutputToError(out, err)
	}

	if strings.Contains(string(out), rule) {
		// The required rule already exist in chain
		return nil
	}

	// TODO: lock on every container but not on chaos-daemon's `/run/xtables.lock`
	cmd = bpm.DefaultProcessBuilder(iptablesCmd, strings.Split("-w "+rule, " ")...).SetNetNS(iptables.nsPath).SetContext(iptables.ctx).Build()
	out, err = cmd.CombinedOutput()
	if err != nil {
		return encodeOutputToError(out, err)
	}

	return nil
}

func (iptables *iptablesClient) flushIptablesChain(chain *iptablesChain) error {
	cmd := bpm.DefaultProcessBuilder(iptablesCmd, "-w", "-F", chain.Name).SetNetNS(iptables.nsPath).SetContext(iptables.ctx).Build()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return encodeOutputToError(out, err)
	}

	return nil
}
