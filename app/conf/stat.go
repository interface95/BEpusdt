package conf

import (
	"fmt"
	"sync"
	"time"
)

const maxRecords = 1000

type stat struct {
	mu      sync.RWMutex
	records []bool
	index   int // 当前位置
	total   int // 已记录总数
	succ    int // 成功记录数
	block   string
	time    int64
}

type info struct {
	Block string `json:"block"`
	Succ  string `json:"succ"`
	Time  int64  `json:"time"`
}

var data sync.Map // map[string]*stat

func getStat(net string) *stat {
	val, _ := data.LoadOrStore(net, &stat{
		records: make([]bool, maxRecords),
	})
	return val.(*stat)
}

func RecordSuccess(net, block string) {
	s := getStat(net)
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.total >= maxRecords && !s.records[s.index] {
		s.succ++
	} else if s.total < maxRecords {
		s.succ++
	}

	s.records[s.index] = true
	s.index = (s.index + 1) % maxRecords
	if s.total < maxRecords {
		s.total++
	}
	s.block = block
	s.time = time.Now().Unix()
}

func RecordFailure(net string) {
	s := getStat(net)
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.total >= maxRecords && s.records[s.index] {
		s.succ--
	}

	s.records[s.index] = false
	s.index = (s.index + 1) % maxRecords
	if s.total < maxRecords {
		s.total++
	}
}

func GetStats() map[string]info {
	var m = make(map[string]info)
	data.Range(func(k, v interface{}) bool {
		s := v.(*stat)
		s.mu.RLock()
		if s.total > 0 {
			m[k.(string)] = info{Block: s.block, Succ: s.successRate(), Time: s.time}
		}
		s.mu.RUnlock()

		return true
	})

	return m
}

func GetSuccessRate(net string) string {
	s := getStat(net)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.successRate()
}

// Caller holds s.mu for a consistent sample and last-success snapshot.
func (s *stat) successRate() string {
	if s.total == 0 {
		return ""
	}

	return fmt.Sprintf("%.2f%%", float64(s.succ)/float64(s.total)*100)
}
