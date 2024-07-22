package validator

import (
	"context"
	"sort"

	"github.com/pkg/errors"
	"github.com/prysmaticlabs/go-bitfield"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/blocks"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/state"
	"github.com/prysmaticlabs/prysm/v5/config/params"
	"github.com/prysmaticlabs/prysm/v5/consensus-types/primitives"
	ethpb "github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1"
	"github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1/attestation"
	"github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1/attestation/aggregation"
	attaggregation "github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1/attestation/aggregation/attestations"
	"github.com/prysmaticlabs/prysm/v5/runtime/version"
	"github.com/prysmaticlabs/prysm/v5/time/slots"
	"go.opencensus.io/trace"
)

type proposerAtts []ethpb.Att

func (vs *Server) packAttestations(ctx context.Context, latestState state.BeaconState, blkSlot primitives.Slot) ([]ethpb.Att, error) {
	ctx, span := trace.StartSpan(ctx, "ProposerServer.packAttestations")
	defer span.End()

	atts := vs.AttPool.AggregatedAttestations()
	atts, err := vs.validateAndDeleteAttsInPool(ctx, latestState, atts)
	if err != nil {
		return nil, errors.Wrap(err, "could not filter attestations")
	}

	uAtts, err := vs.AttPool.UnaggregatedAttestations()
	if err != nil {
		return nil, errors.Wrap(err, "could not get unaggregated attestations")
	}
	uAtts, err = vs.validateAndDeleteAttsInPool(ctx, latestState, uAtts)
	if err != nil {
		return nil, errors.Wrap(err, "could not filter attestations")
	}
	atts = append(atts, uAtts...)

	// Checking the state's version here will give the wrong result if the last slot of Deneb is missed.
	// The head state will still be in Deneb while we are trying to build an Electra block.
	postElectra := slots.ToEpoch(blkSlot) >= params.BeaconConfig().ElectraForkEpoch

	versionAtts := make([]ethpb.Att, 0, len(atts))
	if postElectra {
		for _, a := range atts {
			if a.Version() == version.Electra {
				versionAtts = append(versionAtts, a)
			}
		}
	} else {
		for _, a := range atts {
			if a.Version() == version.Phase0 {
				versionAtts = append(versionAtts, a)
			}
		}
	}

	// Remove duplicates from both aggregated/unaggregated attestations. This
	// prevents inefficient aggregates being created.
	versionAtts, err = proposerAtts(versionAtts).dedup()
	if err != nil {
		return nil, err
	}

	attsById := make(map[attestation.Id][]ethpb.Att, len(versionAtts))
	for _, att := range versionAtts {
		id, err := attestation.NewId(att, attestation.Data)
		if err != nil {
			return nil, errors.Wrap(err, "could not create attestation ID")
		}
		attsById[id] = append(attsById[id], att)
	}

	for id, as := range attsById {
		as, err := attaggregation.Aggregate(as)
		if err != nil {
			return nil, err
		}
		attsById[id] = as
	}

	var attsForInclusion proposerAtts
	if postElectra {
		// TODO: hack for Electra devnet-1, take only one aggregate per ID
		// (which essentially means one aggregate for an attestation_data+committee combination
		topAggregates := make([]ethpb.Att, 0)
		for _, v := range attsById {
			topAggregates = append(topAggregates, v[0])
		}

		attsForInclusion, err = computeOnChainAggregate(topAggregates)
		if err != nil {
			return nil, err
		}
	} else {
		attsForInclusion = make([]ethpb.Att, 0)
		for _, as := range attsById {
			attsForInclusion = append(attsForInclusion, as...)
		}
	}

	deduped, err := attsForInclusion.dedup()
	if err != nil {
		return nil, err
	}
	sorted, err := deduped.sort()
	if err != nil {
		return nil, err
	}
	atts = sorted.limitToMaxAttestations()
	return atts, nil
}

// filter separates attestation list into two groups: valid and invalid attestations.
// The first group passes the all the required checks for attestation to be considered for proposing.
// And attestations from the second group should be deleted.
func (a proposerAtts) filter(ctx context.Context, st state.BeaconState) (proposerAtts, proposerAtts) {
	validAtts := make([]ethpb.Att, 0, len(a))
	invalidAtts := make([]ethpb.Att, 0, len(a))

	for _, att := range a {
		if err := blocks.VerifyAttestationNoVerifySignature(ctx, st, att); err == nil {
			validAtts = append(validAtts, att)
			continue
		}
		invalidAtts = append(invalidAtts, att)
	}
	return validAtts, invalidAtts
}

// sort attestations as follows:
//
//   - all attestations selected by max-cover are ordered before leftover attestations
//   - within selected/leftover, attestations are ordered by slot, with higher slot coming first
//   - within a slot, all top attestations (one per committee) are ordered before any second-best attestations, second-best before third-best etc.
//   - within top/second-best/etc. attestations (one per committee), attestations are ordered by bit count, with higher bit count coming first
func (a proposerAtts) sort() (proposerAtts, error) {
	if len(a) < 2 {
		return a, nil
	}

	type slotAtts struct {
		candidates map[primitives.CommitteeIndex]proposerAtts
		selected   map[primitives.CommitteeIndex]proposerAtts
		leftover   map[primitives.CommitteeIndex]proposerAtts
	}

	// Separate attestations by slot, as slot number takes higher precedence when sorting.
	// Also separate by committee index because maxcover will prefer attestations for the same
	// committee with disjoint bits over attestations for different committees with overlapping
	// bits, even though same bits for different committees are separate votes.
	var slots []primitives.Slot
	attsBySlot := map[primitives.Slot]*slotAtts{}
	for _, att := range a {
		slot := att.GetData().Slot
		ci := att.GetData().CommitteeIndex
		if _, ok := attsBySlot[slot]; !ok {
			attsBySlot[slot] = &slotAtts{}
			attsBySlot[slot].candidates = make(map[primitives.CommitteeIndex]proposerAtts)
			slots = append(slots, slot)
		}
		attsBySlot[slot].candidates[ci] = append(attsBySlot[slot].candidates[ci], att)
	}

	var err error
	for _, sa := range attsBySlot {
		sa.selected = make(map[primitives.CommitteeIndex]proposerAtts)
		sa.leftover = make(map[primitives.CommitteeIndex]proposerAtts)
		for ci, committeeAtts := range sa.candidates {
			sa.selected[ci], sa.leftover[ci], err = committeeAtts.sortByProfitabilityUsingMaxCover()
			if err != nil {
				return nil, err
			}
		}
	}

	var sortedAtts proposerAtts
	sort.Slice(slots, func(i, j int) bool {
		return slots[i] > slots[j]
	})
	for _, slot := range slots {
		sortedAtts = append(sortedAtts, sortSlotAttestations(attsBySlot[slot].selected)...)
	}
	for _, slot := range slots {
		sortedAtts = append(sortedAtts, sortSlotAttestations(attsBySlot[slot].leftover)...)
	}

	return sortedAtts, nil
}

// sortByProfitabilityUsingMaxCover orders attestations by highest aggregation bit count.
// Duplicate bits are counted only once, using max-cover algorithm.
func (a proposerAtts) sortByProfitabilityUsingMaxCover() (selected proposerAtts, leftover proposerAtts, err error) {
	if len(a) < 2 {
		return a, nil, nil
	}
	candidates := make([]*bitfield.Bitlist64, len(a))
	for i := 0; i < len(a); i++ {
		var err error
		candidates[i], err = a[i].GetAggregationBits().ToBitlist64()
		if err != nil {
			return nil, nil, err
		}
	}
	// Add selected candidates on top, those that are not selected - append at bottom.
	selectedKeys, _, err := aggregation.MaxCover(candidates, len(candidates), true /* allowOverlaps */)
	if err != nil {
		log.WithError(err).Debug("MaxCover aggregation failed")
		return a, nil, nil
	}

	// Pick selected attestations first, leftover attestations will be appended at the end.
	// Both lists will be sorted by number of bits set.
	selected = make(proposerAtts, selectedKeys.Count())
	leftover = make(proposerAtts, selectedKeys.Not().Count())
	for i, key := range selectedKeys.BitIndices() {
		selected[i] = a[key]
	}
	for i, key := range selectedKeys.Not().BitIndices() {
		leftover[i] = a[key]
	}
	sort.Slice(selected, func(i, j int) bool {
		return selected[i].GetAggregationBits().Count() > selected[j].GetAggregationBits().Count()
	})
	sort.Slice(leftover, func(i, j int) bool {
		return leftover[i].GetAggregationBits().Count() > leftover[j].GetAggregationBits().Count()
	})
	return selected, leftover, nil
}

// sortSlotAttestations assumes each proposerAtts value in the map is ordered by profitability.
// The function takes the first attestation from each value, orders these attestations by bit count
// and places them at the start of the resulting slice. It then takes the second attestation for each value,
// orders these attestations by bit cound and appends them to the end.
// It continues this pattern until all attestations are processed.
func sortSlotAttestations(slotAtts map[primitives.CommitteeIndex]proposerAtts) proposerAtts {
	attCount := 0
	for _, committeeAtts := range slotAtts {
		attCount += len(committeeAtts)
	}

	sorted := make([]ethpb.Att, 0, attCount)

	processedCount := 0
	index := 0
	for processedCount < attCount {
		var atts []ethpb.Att

		for _, committeeAtts := range slotAtts {
			if len(committeeAtts) > index {
				atts = append(atts, committeeAtts[index])
			}
		}

		sort.Slice(atts, func(i, j int) bool {
			return atts[i].GetAggregationBits().Count() > atts[j].GetAggregationBits().Count()
		})
		sorted = append(sorted, atts...)

		processedCount += len(atts)
		index++
	}

	return sorted
}

// limitToMaxAttestations limits attestations to maximum attestations per block.
func (a proposerAtts) limitToMaxAttestations() proposerAtts {
	if len(a) == 0 {
		return a
	}

	var limit uint64
	if a[0].Version() == version.Phase0 {
		limit = params.BeaconConfig().MaxAttestations
	} else {
		limit = params.BeaconConfig().MaxAttestationsElectra
	}
	if uint64(len(a)) > limit {
		return a[:limit]
	}
	return a
}

// dedup removes duplicate attestations (ones with the same bits set on).
// Important: not only exact duplicates are removed, but proper subsets are removed too
// (their known bits are redundant and are already contained in their supersets).
func (a proposerAtts) dedup() (proposerAtts, error) {
	if len(a) < 2 {
		return a, nil
	}
	attsByDataRoot := make(map[attestation.Id][]ethpb.Att, len(a))
	for _, att := range a {
		id, err := attestation.NewId(att, attestation.Data)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create attestation ID")
		}
		attsByDataRoot[id] = append(attsByDataRoot[id], att)
	}

	uniqAtts := make([]ethpb.Att, 0, len(a))
	for _, atts := range attsByDataRoot {
		for i := 0; i < len(atts); i++ {
			a := atts[i]
			for j := i + 1; j < len(atts); j++ {
				b := atts[j]
				if c, err := a.GetAggregationBits().Contains(b.GetAggregationBits()); err != nil {
					return nil, err
				} else if c {
					// a contains b, b is redundant.
					atts[j] = atts[len(atts)-1]
					atts[len(atts)-1] = nil
					atts = atts[:len(atts)-1]
					j--
				} else if c, err := b.GetAggregationBits().Contains(a.GetAggregationBits()); err != nil {
					return nil, err
				} else if c {
					// b contains a, a is redundant.
					atts[i] = atts[len(atts)-1]
					atts[len(atts)-1] = nil
					atts = atts[:len(atts)-1]
					i--
					break
				}
			}
		}
		uniqAtts = append(uniqAtts, atts...)
	}

	return uniqAtts, nil
}

// This filters the input attestations to return a list of valid attestations to be packaged inside a beacon block.
func (vs *Server) validateAndDeleteAttsInPool(ctx context.Context, st state.BeaconState, atts []ethpb.Att) ([]ethpb.Att, error) {
	ctx, span := trace.StartSpan(ctx, "ProposerServer.validateAndDeleteAttsInPool")
	defer span.End()

	validAtts, invalidAtts := proposerAtts(atts).filter(ctx, st)
	if err := vs.deleteAttsInPool(ctx, invalidAtts); err != nil {
		return nil, err
	}
	return validAtts, nil
}

// The input attestations are processed and seen by the node, this deletes them from pool
// so proposers don't include them in a block for the future.
func (vs *Server) deleteAttsInPool(ctx context.Context, atts []ethpb.Att) error {
	ctx, span := trace.StartSpan(ctx, "ProposerServer.deleteAttsInPool")
	defer span.End()

	for _, att := range atts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if helpers.IsAggregated(att) {
			if err := vs.AttPool.DeleteAggregatedAttestation(att); err != nil {
				return err
			}
		} else {
			if err := vs.AttPool.DeleteUnaggregatedAttestation(att); err != nil {
				return err
			}
		}
	}
	return nil
}
