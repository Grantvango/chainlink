package s4

import (
	"sort"
	"sync"
	"time"

	"github.com/smartcontractkit/chainlink/v2/core/services/pg"
	"github.com/smartcontractkit/chainlink/v2/core/utils"
)

type key struct {
	address string
	slot    uint
}

type inMemoryOrm struct {
	rows map[key]*Row
	mu   sync.RWMutex
}

var _ ORM = (*inMemoryOrm)(nil)

func NewInMemoryORM() ORM {
	return &inMemoryOrm{
		rows: make(map[key]*Row),
	}
}

func (o *inMemoryOrm) Get(address *utils.Big, slotId uint, qopts ...pg.QOpt) (*Row, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	mkey := key{
		address: address.Hex(),
		slot:    slotId,
	}
	row, ok := o.rows[mkey]
	if !ok {
		return nil, ErrNotFound
	}
	return row.Clone(), nil
}

func (o *inMemoryOrm) Update(row *Row, qopts ...pg.QOpt) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	mkey := key{
		address: row.Address.Hex(),
		slot:    row.SlotId,
	}
	existing, ok := o.rows[mkey]
	if ok && existing.Version > row.Version {
		return ErrVersionTooLow
	}

	clone := row.Clone()
	clone.UpdatedAt = time.Now().UnixMilli()
	o.rows[mkey] = clone
	return nil
}

func (o *inMemoryOrm) DeleteExpired(limit uint, now time.Time, qopts ...pg.QOpt) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	queue := make([]key, 0)
	for k, v := range o.rows {
		if v.Expiration < now.UnixMilli() {
			queue = append(queue, k)
			if len(queue) >= int(limit) {
				break
			}
		}
	}
	for _, k := range queue {
		delete(o.rows, k)
	}

	return nil
}

func (o *inMemoryOrm) GetSnapshot(addressRange *AddressRange, qopts ...pg.QOpt) ([]*SnapshotRow, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	now := time.Now().UnixMilli()
	var rows []*SnapshotRow
	for _, row := range o.rows {
		if row.Expiration > now {
			rows = append(rows, &SnapshotRow{
				Address: utils.NewBig(row.Address.ToInt()),
				SlotId:  row.SlotId,
				Version: row.Version,
			})
		}
	}

	return rows, nil
}

func (o *inMemoryOrm) GetUnconfirmedRows(limit uint, qopts ...pg.QOpt) ([]*Row, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	now := time.Now().UnixMilli()
	var rows []*Row
	for _, row := range o.rows {
		if row.Expiration > now && !row.Confirmed {
			rows = append(rows, row)
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].UpdatedAt < rows[j].UpdatedAt
	})

	if uint(len(rows)) > limit {
		rows = rows[:limit]
	}

	for i, row := range rows {
		rows[i] = row.Clone()
	}

	return rows, nil
}