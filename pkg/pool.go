package tunnel

import (
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
)

type PoolInts struct {
	free      map[int]bool
	allocated map[int]bool
	tail      int

	mtx sync.Mutex
}

func NewPoolForRange(lowerBound, cap int) *PoolInts {
	p := &PoolInts{
		free:      make(map[int]bool),
		allocated: make(map[int]bool),
	}
	for i := 0; i < cap; i = i + 1 {
		p.free[lowerBound+i] = true
	}

	return p
}

func (p *PoolInts) GetElement(v int) (int, error) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if len(p.free) == 0 {
		return 0, fmt.Errorf("No more elements available")
	}

	if v == 0 {
		for k := range p.free {
			v = k
			break
		}
	} else {
		if ok, _ := p.free[v]; !ok {
			return 0, fmt.Errorf("Explicit element not available")
		}
	}

	delete(p.free, v)
	p.allocated[v] = true

	log.Debugf("Pool GetElement: [%d] => (free: %d)", v, len(p.free))

	return v, nil
}

func (p *PoolInts) ReturnElement(v int) error {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if ok, _ := p.allocated[v]; ok {
		delete(p.allocated, v)
		p.free[v] = true
	}

	log.Debugf("Pool ReturnElement: [%d] => (free:%d)", len(p.free))

	return nil
}
