/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package plan

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/google/go-cmp/cmp"
	log "github.com/sirupsen/logrus"
	"k8s.io/utils/strings/slices"

	"sigs.k8s.io/external-dns/endpoint"
)

// PropertyComparator is used in Plan for comparing the previous and current custom annotations.
type PropertyComparator func(name string, previous string, current string) bool

// Plan can convert a list of desired and current records to a series of create,
// update and delete actions.
type Plan struct {
	// List of current records
	Current []*endpoint.Endpoint
	// List of desired records
	Desired []*endpoint.Endpoint
	// List of missing records to be created, use for the migrations (e.g. old-new TXT format)
	Missing []*endpoint.Endpoint
	// Policies under which the desired changes are calculated
	Policies []Policy
	// List of changes necessary to move towards desired state
	// Populated after calling Calculate()
	Changes *Changes
	// DomainFilter matches DNS names
	DomainFilter endpoint.DomainFilterInterface
	// Property comparator compares custom properties of providers
	PropertyComparator PropertyComparator
	// DNS record types that will be considered for management
	ManagedRecords []string
	// current txt-owner
	TXTOwner string
	// has migrate txt-owner
	HasMig bool
	// modify owner flag
	TXTOwnerMigrate bool
	// old txt-owner whitch needed to modify
	TXTOwnerOld string
}

// Changes holds lists of actions to be executed by dns providers
type Changes struct {
	// Records that need to be created
	Create []*endpoint.Endpoint
	// Records that need to be updated (current data)
	UpdateOld []*endpoint.Endpoint
	// Records that need to be updated (desired data)
	UpdateNew []*endpoint.Endpoint
	// Records that need to be deleted
	Delete []*endpoint.Endpoint
}

// planKey is a key for a row in `planTable`.
type planKey struct {
	dnsName       string
	setIdentifier string
	recordType    string
}

// planTable is a supplementary struct for Plan
// each row correspond to a planKey -> (current record + all desired records)
/*
planTable: (-> = target)
--------------------------------------------------------
DNSName | Current record | Desired Records             |
--------------------------------------------------------
foo.com | -> 1.1.1.1     | [->1.1.1.1, ->elb.com]      |  = no action
--------------------------------------------------------
bar.com |                | [->191.1.1.1, ->190.1.1.1]  |  = create (bar.com -> 190.1.1.1)
--------------------------------------------------------
"=", i.e. result of calculation relies on supplied ConflictResolver
*/
type planTable struct {
	rows     map[planKey]*planTableRow
	resolver ConflictResolver
}

func newPlanTable() planTable { // TODO: make resolver configurable
	return planTable{map[planKey]*planTableRow{}, PerResource{}}
}

// planTableRow
// current corresponds to the record currently occupying dns name on the dns provider
// candidates corresponds to the list of records which would like to have this dnsName
type planTableRow struct {
	current    *endpoint.Endpoint
	candidates []*endpoint.Endpoint
}

func (t planTableRow) String() string {
	return fmt.Sprintf("planTableRow{current=%v, candidates=%v}", t.current, t.candidates)
}

func (t planTable) addCurrent(e *endpoint.Endpoint) {
	key := t.newPlanKey(e)
	t.rows[key].current = e
}

func (t planTable) addCandidate(e *endpoint.Endpoint) {
	key := t.newPlanKey(e)
	t.rows[key].candidates = append(t.rows[key].candidates, e)
}

func (t *planTable) newPlanKey(e *endpoint.Endpoint) planKey {
	key := planKey{
		dnsName:       normalizeDNSName(e.DNSName),
		setIdentifier: e.SetIdentifier,
		recordType:    e.RecordType,
	}
	if _, ok := t.rows[key]; !ok {
		t.rows[key] = &planTableRow{}
	}
	return key
}

func (c *Changes) HasChanges() bool {
	if len(c.Create) > 0 || len(c.Delete) > 0 {
		return true
	}
	return !cmp.Equal(c.UpdateNew, c.UpdateOld)
}

// Calculate computes the actions needed to move current state towards desired
// state. It then passes those changes to the current policy for further
// processing. It returns a copy of Plan with the changes populated.
func (p *Plan) Calculate() *Plan {
	t := newPlanTable()

	if p.DomainFilter == nil {
		p.DomainFilter = endpoint.MatchAllDomainFilters(nil)
	}

	for _, current := range filterRecordsForPlan(p.Current, p.DomainFilter, p.ManagedRecords) {
		t.addCurrent(current)
	}
	for _, desired := range filterRecordsForPlan(p.Desired, p.DomainFilter, p.ManagedRecords) {
		t.addCandidate(desired)
	}

	changes := &Changes{}
	var hasMig bool

	for _, row := range t.rows {
		if row.current == nil { // dns name not taken
			changes.Create = append(changes.Create, t.resolver.ResolveCreate(row.candidates))
		}
		if row.current != nil && len(row.candidates) == 0 {
			changes.Delete = append(changes.Delete, row.current)
		}

		// Change the specified old txt-owner to the new txt-owner (if TXTOwnerMigrate==true and set the from-txt-owner)
		if p.TXTOwnerMigrate && row.current != nil && len(row.candidates) > 0 && row.current.Labels[endpoint.OwnerLabelKey] == p.TXTOwnerOld {
			hasMig = true
			oldOwner := row.current.Labels[endpoint.OwnerLabelKey]
			update := row.current.DeepCopy()
			update.Labels[endpoint.OwnerLabelKey] = p.TXTOwner
			// log.Infof("find a record owner!=%s: it's DNSName: %s, old-owner: %s, type: %s\n", p.TXTOwner, row.current.DNSName, oldOwner, row.current.RecordType)
			log.WithFields(log.Fields{
				"previousOwner": oldOwner,
				"newOwner":      p.TXTOwner,
				"dnsName":       row.current.DNSName,
				"recordType":    row.current.RecordType,
			}).Info("Found record to migrate")
			changes.UpdateNew = append(changes.UpdateNew, update)
			changes.UpdateOld = append(changes.UpdateOld, row.current)
			continue
		}

		// TODO: allows record type change, which might not be supported by all dns providers
		if row.current != nil && len(row.candidates) > 0 { // dns name is taken
			update := t.resolver.ResolveUpdate(row.current, row.candidates)
			// compare "update" to "current" to figure out if actual update is required
			if shouldUpdateTTL(update, row.current) || targetChanged(update, row.current) || p.shouldUpdateProviderSpecific(update, row.current) {
				inheritOwner(row.current, update)
				changes.UpdateNew = append(changes.UpdateNew, update)
				changes.UpdateOld = append(changes.UpdateOld, row.current)
			}
			continue
		}
	}
	for _, pol := range p.Policies {
		changes = pol.Apply(changes)
	}

	// Handle the migration of the TXT records created before the new format (introduced in v0.12.0)
	if len(p.Missing) > 0 {
		changes.Create = append(changes.Create, filterRecordsForPlan(p.Missing, p.DomainFilter, append(p.ManagedRecords, endpoint.RecordTypeTXT))...)
	}

	plan := &Plan{
		Current: p.Current,
		Desired: p.Desired,
		Changes: &Changes{
			Create:    changes.Create,
			UpdateNew: filterOwnedRecords(p.TXTOwner, p.TXTOwnerOld, p.TXTOwnerMigrate, changes.UpdateNew),
			UpdateOld: filterOwnedRecords(p.TXTOwner, p.TXTOwnerOld, p.TXTOwnerMigrate, changes.UpdateOld),
			Delete:    filterOwnedRecords(p.TXTOwner, p.TXTOwnerOld, p.TXTOwnerMigrate, changes.Delete),
		},
		ManagedRecords: []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME},
		HasMig:         hasMig,
	}

	return plan
}

func inheritOwner(from, to *endpoint.Endpoint) {
	if to.Labels == nil {
		to.Labels = map[string]string{}
	}
	if from.Labels == nil {
		from.Labels = map[string]string{}
	}
	to.Labels[endpoint.OwnerLabelKey] = from.Labels[endpoint.OwnerLabelKey]
}

func targetChanged(desired, current *endpoint.Endpoint) bool {
	return !desired.Targets.Same(current.Targets)
}

func shouldUpdateTTL(desired, current *endpoint.Endpoint) bool {
	if !desired.RecordTTL.IsConfigured() {
		return false
	}
	return desired.RecordTTL != current.RecordTTL
}

func (p *Plan) shouldUpdateProviderSpecific(desired, current *endpoint.Endpoint) bool {
	desiredProperties := map[string]endpoint.ProviderSpecificProperty{}

	if desired.ProviderSpecific != nil {
		for _, d := range desired.ProviderSpecific {
			desiredProperties[d.Name] = d
		}
	}
	if current.ProviderSpecific != nil {
		for _, c := range current.ProviderSpecific {
			if d, ok := desiredProperties[c.Name]; ok {
				if p.PropertyComparator != nil {
					if !p.PropertyComparator(c.Name, c.Value, d.Value) {
						return true
					}
				} else if c.Value != d.Value {
					return true
				}
			} else {
				if p.PropertyComparator != nil {
					if !p.PropertyComparator(c.Name, c.Value, "") {
						return true
					}
				} else if c.Value != "" {
					return true
				}
			}
		}
	}

	return false
}

// TODO(ideahitme): consider moving this to Plan
func filterOwnedRecords(ownerID string, ownerIDOld string, migrate bool, eps []*endpoint.Endpoint) []*endpoint.Endpoint {
	filtered := []*endpoint.Endpoint{}
	ownerIDs := []string{ownerID}

	if migrate {
		ownerIDs = append(ownerIDs, ownerIDOld)
	}

	for _, ep := range eps {
		if endpointOwner, ok := ep.Labels[endpoint.OwnerLabelKey]; !ok || !slices.Contains(ownerIDs, endpointOwner) {
			log.Debugf(`Skipping endpoint %v because owner id does not match, found: "%s", required: "%s"`, ep, endpointOwner, ownerIDs)
			continue
		}
		filtered = append(filtered, ep)
	}

	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// filterRecordsForPlan removes records that are not relevant to the planner.
// Currently this just removes TXT records to prevent them from being
// deleted erroneously by the planner (only the TXT registry should do this.)
//
// Per RFC 1034, CNAME records conflict with all other records - it is the
// only record with this property. The behavior of the planner may need to be
// made more sophisticated to codify this.
func filterRecordsForPlan(records []*endpoint.Endpoint, domainFilter endpoint.DomainFilterInterface, managedRecords []string) []*endpoint.Endpoint {
	filtered := []*endpoint.Endpoint{}

	for _, record := range records {
		// Ignore records that do not match the domain filter provided
		if !domainFilter.Match(record.DNSName) {
			log.Debugf("ignoring record %s that does not match domain filter", record.DNSName)
			continue
		}
		if IsManagedRecord(record.RecordType, managedRecords) {
			filtered = append(filtered, record)
		}
	}

	return filtered
}

// normalizeDNSName converts a DNS name to a canonical form, so that we can use string equality
// it: removes space, converts to lower case, ensures there is a trailing dot
func normalizeDNSName(dnsName string) string {
	s := strings.TrimSpace(strings.ToLower(dnsName))
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// CompareBoolean is an implementation of PropertyComparator for comparing boolean-line values
// For example external-dns.alpha.kubernetes.io/cloudflare-proxied: "true"
// If value doesn't parse as boolean, the defaultValue is used
func CompareBoolean(defaultValue bool, name, current, previous string) bool {
	var err error

	v1, v2 := defaultValue, defaultValue

	if previous != "" {
		v1, err = strconv.ParseBool(previous)
		if err != nil {
			v1 = defaultValue
		}
	}

	if current != "" {
		v2, err = strconv.ParseBool(current)
		if err != nil {
			v2 = defaultValue
		}
	}

	return v1 == v2
}

func IsManagedRecord(record string, managedRecords []string) bool {
	for _, r := range managedRecords {
		if record == r {
			return true
		}
	}
	return false
}
