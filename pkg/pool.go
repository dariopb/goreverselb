package tunnel

import (
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
)

type PoolInts struct {
	buckets []int
	head    int
	tail    int

	mtx sync.Mutex
}

func NewPoolForRange(lowerBound, cap int) *PoolInts {
	p := &PoolInts{
		buckets: make([]int, cap+1),
		head:    0,
		tail:    cap,
	}
	for i := 0; i < cap; i = i + 1 {
		p.buckets[i] = lowerBound + i
	}

	return p
}

func (p *PoolInts) GetElement() (int, error) {
	p.mtx.Lock()

	if p.head == p.tail {
		p.mtx.Unlock()
		return 0, fmt.Errorf("No more elements available")
	}
	v := p.buckets[p.head]
	p.head = (p.head + 1) % len(p.buckets)

	log.Debug("Pool GetElement: [%d] => (%d:%d)", v, p.head, p.tail)

	p.mtx.Unlock()
	return v, nil
}

func (p *PoolInts) ReturnElement(v int) error {
	p.mtx.Lock()

	if p.tail == (p.head+1)%len(p.buckets) {
		p.mtx.Unlock()
		return fmt.Errorf("Capacity exceeded")
	}
	p.buckets[p.tail] = v
	p.tail = (p.tail + 1) % len(p.buckets)

	log.Debug("Pool ReturnElement: [%d] => (%d:%d)", v, p.head, p.tail)

	p.mtx.Unlock()
	return nil
}
