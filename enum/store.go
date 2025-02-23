// Copyright © by Jeff Foley 2017-2022. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package enum

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	amassnet "github.com/OWASP/Amass/v3/net"
	amassdns "github.com/OWASP/Amass/v3/net/dns"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/caffix/pipeline"
	"github.com/caffix/queue"
	"github.com/caffix/resolve"
	"github.com/miekg/dns"
	bf "github.com/tylertreat/BoomFilters"
	"golang.org/x/net/publicsuffix"
)

// dataManager is the stage that stores all data processed by the pipeline.
type dataManager struct {
	enum        *Enumeration
	queue       queue.Queue
	signalDone  chan struct{}
	confirmDone chan struct{}
	filter      *bf.StableBloomFilter
}

// newDataManager returns a dataManager specific to the provided Enumeration.
func newDataManager(e *Enumeration) *dataManager {
	dm := &dataManager{
		enum:        e,
		queue:       queue.NewQueue(),
		signalDone:  make(chan struct{}, 2),
		confirmDone: make(chan struct{}, 2),
		filter:      bf.NewDefaultStableBloomFilter(100000, 0.01),
	}

	go dm.processASNRequests()
	return dm
}

func (dm *dataManager) Stop() chan struct{} {
	dm.filter.Reset()
	close(dm.signalDone)
	return dm.confirmDone
}

// Process implements the pipeline Task interface.
func (dm *dataManager) Process(ctx context.Context, data pipeline.Data, tp pipeline.TaskParams) (pipeline.Data, error) {
	select {
	case <-ctx.Done():
		return nil, nil
	default:
	}

	var id string
	switch v := data.(type) {
	case *requests.DNSRequest:
		if v == nil {
			return nil, nil
		}

		id = v.Name
		if err := dm.dnsRequest(ctx, v, tp); err != nil {
			dm.enum.Config.Log.Print(err.Error())
		}
	case *requests.AddrRequest:
		if v == nil {
			return nil, nil
		}

		id = v.Address
		if err := dm.addrRequest(ctx, v, tp); err != nil {
			dm.enum.Config.Log.Print(err.Error())
		}
	}

	if id != "" && dm.filter.TestAndAdd([]byte(id)) {
		return nil, nil
	}
	return data, nil
}

func (dm *dataManager) dnsRequest(ctx context.Context, req *requests.DNSRequest, tp pipeline.TaskParams) error {
	if dm.enum.Config.Blacklisted(req.Name) {
		return nil
	}
	// Check for CNAME records first
	for i, r := range req.Records {
		req.Records[i].Name = strings.Trim(strings.ToLower(r.Name), ".")
		req.Records[i].Data = strings.Trim(strings.ToLower(r.Data), ".")

		if uint16(r.Type) == dns.TypeCNAME {
			// Do not enter more than the CNAME record
			return dm.insertCNAME(ctx, req, i, tp)
		}
	}

	var err error
	for i, r := range req.Records {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		switch uint16(r.Type) {
		case dns.TypeA:
			err = dm.insertA(ctx, req, i, tp)
		case dns.TypeAAAA:
			err = dm.insertAAAA(ctx, req, i, tp)
		case dns.TypePTR:
			err = dm.insertPTR(ctx, req, i, tp)
		case dns.TypeSRV:
			err = dm.insertSRV(ctx, req, i, tp)
		case dns.TypeNS:
			err = dm.insertNS(ctx, req, i, tp)
		case dns.TypeMX:
			err = dm.insertMX(ctx, req, i, tp)
		case dns.TypeTXT:
			err = dm.insertTXT(ctx, req, i, tp)
		case dns.TypeSOA:
			err = dm.insertSOA(ctx, req, i, tp)
		case dns.TypeSPF:
			err = dm.insertSPF(ctx, req, i, tp)
		}
		if err != nil {
			break
		}
	}
	return err
}

func (dm *dataManager) insertCNAME(ctx context.Context, req *requests.DNSRequest, recidx int, tp pipeline.TaskParams) error {
	target := resolve.RemoveLastDot(req.Records[recidx].Data)
	if target == "" {
		return errors.New("failed to extract a FQDN from the DNS answer data")
	}

	domain, err := publicsuffix.EffectiveTLDPlusOne(target)
	if err != nil || domain == "" {
		return errors.New("failed to extract a domain name from the FQDN")
	}
	// Important - Allows chained CNAME records to be resolved until an A/AAAA record
	dm.enum.nameSrc.pipelineData(ctx, &requests.DNSRequest{
		Name:   target,
		Domain: strings.ToLower(domain),
		Tag:    requests.DNS,
		Source: "DNS",
	}, tp)
	if err := dm.enum.graph.UpsertCNAME(ctx, req.Name, target, req.Source, dm.enum.Config.UUID.String()); err != nil {
		return fmt.Errorf("%s failed to insert CNAME: %v", dm.enum.graph, err)
	}
	return nil
}

func (dm *dataManager) insertA(ctx context.Context, req *requests.DNSRequest, recidx int, tp pipeline.TaskParams) error {
	addr := strings.TrimSpace(req.Records[recidx].Data)
	if addr == "" {
		return errors.New("failed to extract an IP address from the DNS answer data")
	}
	dm.enum.checkForMissedWildcards(addr)
	dm.enum.nameSrc.pipelineData(ctx, &requests.AddrRequest{
		Address: addr,
		InScope: true,
		Domain:  req.Domain,
		Tag:     requests.DNS,
		Source:  "DNS",
	}, tp)
	if err := dm.enum.graph.UpsertA(ctx, req.Name, addr, req.Source, dm.enum.Config.UUID.String()); err != nil {
		return fmt.Errorf("%s failed to insert A record: %v", dm.enum.graph, err)
	}
	return nil
}

func (dm *dataManager) insertAAAA(ctx context.Context, req *requests.DNSRequest, recidx int, tp pipeline.TaskParams) error {
	addr := strings.TrimSpace(req.Records[recidx].Data)
	if addr == "" {
		return errors.New("failed to extract an IP address from the DNS answer data")
	}
	dm.enum.checkForMissedWildcards(addr)
	dm.enum.nameSrc.pipelineData(ctx, &requests.AddrRequest{
		Address: addr,
		InScope: true,
		Domain:  req.Domain,
		Tag:     requests.DNS,
		Source:  "DNS",
	}, tp)
	if err := dm.enum.graph.UpsertAAAA(ctx, req.Name, addr, req.Source, dm.enum.Config.UUID.String()); err != nil {
		return fmt.Errorf("%s failed to insert AAAA record: %v", dm.enum.graph, err)
	}
	return nil
}

func (dm *dataManager) insertPTR(ctx context.Context, req *requests.DNSRequest, recidx int, tp pipeline.TaskParams) error {
	target := resolve.RemoveLastDot(req.Records[recidx].Data)
	if target == "" {
		return errors.New("failed to extract a FQDN from the DNS answer data")
	}
	// Do not go further if the target is not in scope
	domain := strings.ToLower(dm.enum.Config.WhichDomain(target))
	if domain == "" {
		return nil
	}
	// Important - Allows the target DNS name to be resolved in the forward direction
	dm.enum.nameSrc.pipelineData(ctx, &requests.DNSRequest{
		Name:   target,
		Domain: domain,
		Tag:    requests.DNS,
		Source: "Reverse DNS",
	}, tp)
	if err := dm.enum.graph.UpsertPTR(ctx, req.Name, target, req.Source, dm.enum.Config.UUID.String()); err != nil {
		return fmt.Errorf("%s failed to insert PTR record: %v", dm.enum.graph, err)
	}
	return nil
}

func (dm *dataManager) insertSRV(ctx context.Context, req *requests.DNSRequest, recidx int, tp pipeline.TaskParams) error {
	service := resolve.RemoveLastDot(req.Records[recidx].Name)
	target := resolve.RemoveLastDot(req.Records[recidx].Data)
	if target == "" || service == "" {
		return errors.New("failed to extract service info from the DNS answer data")
	}
	if domain := dm.enum.Config.WhichDomain(target); domain != "" {
		dm.enum.nameSrc.pipelineData(ctx, &requests.DNSRequest{
			Name:   target,
			Domain: domain,
			Tag:    requests.DNS,
			Source: "DNS",
		}, tp)
	}
	if err := dm.enum.graph.UpsertSRV(ctx, req.Name, service, target, req.Source, dm.enum.Config.UUID.String()); err != nil {
		return fmt.Errorf("%s failed to insert SRV record: %v", dm.enum.graph, err)
	}
	return nil
}

func (dm *dataManager) insertNS(ctx context.Context, req *requests.DNSRequest, recidx int, tp pipeline.TaskParams) error {
	target := req.Records[recidx].Data
	if target == "" {
		return errors.New("failed to extract NS info from the DNS answer data")
	}

	domain, err := publicsuffix.EffectiveTLDPlusOne(target)
	if err != nil || domain == "" {
		return errors.New("failed to extract a domain name from the FQDN")
	}
	if d := strings.ToLower(domain); target != d {
		dm.enum.nameSrc.pipelineData(ctx, &requests.DNSRequest{
			Name:   target,
			Domain: d,
			Tag:    requests.DNS,
			Source: "DNS",
		}, tp)
	}
	if err := dm.enum.graph.UpsertNS(ctx, req.Name, target, req.Source, dm.enum.Config.UUID.String()); err != nil {
		return fmt.Errorf("%s failed to insert NS record: %v", dm.enum.graph, err)
	}
	return nil
}

func (dm *dataManager) insertMX(ctx context.Context, req *requests.DNSRequest, recidx int, tp pipeline.TaskParams) error {
	target := resolve.RemoveLastDot(req.Records[recidx].Data)
	if target == "" {
		return errors.New("failed to extract a FQDN from the DNS answer data")
	}

	domain, err := publicsuffix.EffectiveTLDPlusOne(target)
	if err != nil || domain == "" {
		return errors.New("failed to extract a domain name from the FQDN")
	}
	if d := strings.ToLower(domain); target != d {
		dm.enum.nameSrc.pipelineData(ctx, &requests.DNSRequest{
			Name:   target,
			Domain: d,
			Tag:    requests.DNS,
			Source: "DNS",
		}, tp)
	}
	if err := dm.enum.graph.UpsertMX(ctx, req.Name, target, req.Source, dm.enum.Config.UUID.String()); err != nil {
		return fmt.Errorf("%s failed to insert MX record: %v", dm.enum.graph, err)
	}
	return nil
}

func (dm *dataManager) insertTXT(ctx context.Context, req *requests.DNSRequest, recidx int, tp pipeline.TaskParams) error {
	if dm.enum.Config.IsDomainInScope(req.Name) {
		dm.findNamesAndAddresses(ctx, req.Records[recidx].Data, req.Domain, tp)
	}
	return nil
}

func (dm *dataManager) insertSOA(ctx context.Context, req *requests.DNSRequest, recidx int, tp pipeline.TaskParams) error {
	if dm.enum.Config.IsDomainInScope(req.Name) {
		dm.findNamesAndAddresses(ctx, req.Records[recidx].Data, req.Domain, tp)
	}
	return nil
}

func (dm *dataManager) insertSPF(ctx context.Context, req *requests.DNSRequest, recidx int, tp pipeline.TaskParams) error {
	if dm.enum.Config.IsDomainInScope(req.Name) {
		dm.findNamesAndAddresses(ctx, req.Records[recidx].Data, req.Domain, tp)
	}
	return nil
}

func (dm *dataManager) findNamesAndAddresses(ctx context.Context, data, domain string, tp pipeline.TaskParams) {
	ipre := regexp.MustCompile(amassnet.IPv4RE)
	for _, ip := range ipre.FindAllString(data, -1) {
		dm.enum.nameSrc.pipelineData(ctx, &requests.AddrRequest{
			Address: ip,
			Domain:  domain,
			Tag:     requests.DNS,
			Source:  "DNS",
		}, tp)
	}

	subre := amassdns.AnySubdomainRegex()
	for _, name := range subre.FindAllString(data, -1) {
		if domain := strings.ToLower(dm.enum.Config.WhichDomain(name)); domain != "" {
			dm.enum.nameSrc.pipelineData(ctx, &requests.DNSRequest{
				Name:   name,
				Domain: domain,
				Tag:    requests.DNS,
				Source: "DNS",
			}, tp)
		}
	}
}

type queuedAddrRequest struct {
	Ctx context.Context
	Req *requests.AddrRequest
}

func (dm *dataManager) addrRequest(ctx context.Context, req *requests.AddrRequest, tp pipeline.TaskParams) error {
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	uuid := dm.enum.Config.UUID.String()
	if req == nil || !req.InScope || uuid == "" {
		return nil
	}
	if yes, prefix := amassnet.IsReservedAddress(req.Address); yes {
		var err error
		if e := dm.enum.graph.UpsertInfrastructure(ctx, 0,
			amassnet.ReservedCIDRDescription, req.Address, prefix, "RIR", uuid); e != nil {
			err = e
		}
		return err
	}
	if r := dm.enum.Sys.Cache().AddrSearch(req.Address); r != nil {
		var err error
		if e := dm.enum.graph.UpsertInfrastructure(ctx, r.ASN,
			r.Description, req.Address, r.Prefix, r.Source, uuid); e != nil {
			err = e
		}
		return err
	}

	dm.queue.Append(&queuedAddrRequest{
		Ctx: ctx,
		Req: req,
	})
	return nil
}

func (dm *dataManager) processASNRequests() {
loop:
	for {
		select {
		case <-dm.signalDone:
			if dm.queue.Len() == 0 {
				break loop
			}
			dm.nextInfraInfo()
		case <-dm.queue.Signal():
			dm.nextInfraInfo()
		}
	}
	close(dm.confirmDone)
}

func (dm *dataManager) nextInfraInfo() {
	e, ok := dm.queue.Next()
	if !ok {
		return
	}
	qar := e.(*queuedAddrRequest)

	ctx := qar.Ctx
	req := qar.Req
	uuid := dm.enum.Config.UUID.String()
	if r := dm.enum.Sys.Cache().AddrSearch(req.Address); r != nil {
		_ = dm.enum.graph.UpsertInfrastructure(ctx, r.ASN, r.Description, req.Address, r.Prefix, r.Source, uuid)
		return
	}

	dm.enum.sendRequests(&requests.ASNRequest{Address: req.Address})

	for i := 0; i < 30; i++ {
		if r := dm.enum.Sys.Cache().AddrSearch(req.Address); r != nil {
			_ = dm.enum.graph.UpsertInfrastructure(ctx, r.ASN, r.Description, req.Address, r.Prefix, r.Source, uuid)
			return
		}
		time.Sleep(time.Second)
	}

	asn := 0
	desc := "Unknown"
	prefix := fakePrefix(req.Address)
	_ = dm.enum.graph.UpsertInfrastructure(ctx, asn, desc, req.Address, prefix, "RIR", uuid)

	first, cidr, _ := net.ParseCIDR(prefix)
	dm.enum.Sys.Cache().Update(&requests.ASNRequest{
		Address:     first.String(),
		ASN:         asn,
		Prefix:      cidr.String(),
		Description: desc,
		Tag:         requests.RIR,
		Source:      "RIR",
	})
}

func fakePrefix(addr string) string {
	bits := 24
	total := 32
	ip := net.ParseIP(addr)

	if amassnet.IsIPv6(ip) {
		bits = 48
		total = 128
	}

	mask := net.CIDRMask(bits, total)
	return fmt.Sprintf("%s/%d", ip.Mask(mask).String(), bits)
}
