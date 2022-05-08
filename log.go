package raft

import (
	"errors"
	"sync"
	"time"
)

// Log raft log
type Log interface {
	// Get 获取 raft log 中索引为 index 的 log entry term
	// 若无, 则返回 0, nil
	Get(index uint64) (term uint64, err error)
	// Match 是否有匹配上 term 与 index 的 log entry
	Match(index, term uint64) (bool, error)
	// Last 返回最后一个 log entry 的 term 与 index
	// 若无, 则返回 0 , 0
	Last() (index, term uint64, err error)
	// RangeGet 获取在 (i, j] 索引区间的 log entry
	// 若无, 则返回 nil, nil
	RangeGet(i, j uint64) ([]LogEntry, error)
	// Append 追加 log entry
	Append(entries ...LogEntry) error
	// AppendAt 在 prevIndex 之后追加 log entry
	AppendAt(prevIndex uint64, entries ...LogEntry) error
	// AppendEntry append entry and return entry index
	AppendEntry(entry LogEntry) (index uint64, err error)
}

// LogEntryType log entry type
// represents various purpose of use
type LogEntryType uint8

const (
	// logEntryTypeCommand represents command that applying to replicated state matchine
	logEntryTypeCommand LogEntryType = iota
	// LogEntryTypeClusterConfiguration represents a cluster configuration
	// is created when a raft peer is added, removed, etc
	logEntryTypeClusterConfiguration
)

// LogEntry raft log entry
//	each entry contains command for state machine,
//	and term when entry was received by leader (first index is 1)
type LogEntry struct {
	Index      uint64
	Term       uint64
	Type       LogEntryType
	Command    Command
	AppendTime time.Time
}

var _ Log = (*memoryLog)(nil)

// memoryLog just for testing
type memoryLog struct {
	mux   sync.Mutex
	queue []LogEntry
}

// Get 获取 raft log 中索引为 index 的 log entry term
// 若无, 则返回 0, nil
func (l *memoryLog) Get(index uint64) (term uint64, err error) {
	l.mux.Lock()
	defer l.mux.Unlock()

	if index == 0 {
		return 0, nil
	}

	index--

	length := uint64(len(l.queue))
	if index >= 0 && index < length {
		entry := l.queue[index]
		return entry.Term, nil
	}
	return 0, nil
}

// Match 是否有匹配上 term 与 index 的 log entry
func (l *memoryLog) Match(index, term uint64) (bool, error) {
	target, err := l.Get(index)
	if err != nil {
		return false, err
	}

	return term == target, nil
}

// Last 返回最后一个 log entry 的 term 与 index
// 若无, 则返回 0 , 0
func (l *memoryLog) Last() (index, term uint64, err error) {
	l.mux.Lock()
	defer l.mux.Unlock()

	return l.last()
}

func (l *memoryLog) last() (index, term uint64, err error) {
	if len(l.queue) == 0 {
		return 0, 0, nil
	}

	entry := l.queue[len(l.queue)-1]
	return entry.Index, entry.Term, nil
}

// RangeGet 获取在 (i, j] 索引区间的 log entry
// 若无, 则返回 nil, nil
func (l *memoryLog) RangeGet(i, j uint64) ([]LogEntry, error) {
	l.mux.Lock()
	defer l.mux.Unlock()

	if j <= i {
		return nil, nil
	}

	i--
	j--
	var entries []LogEntry
	for k := i + 1; k <= j && k < uint64(len(l.queue)); k++ {
		entries = append(entries, l.queue[k])
	}
	return entries, nil
}

func (l *memoryLog) popAfter(i uint64) error {
	if i == uint64(len(l.queue)) {
		return nil
	}
	if i > uint64(len(l.queue)) {
		return errors.New("index out of log's range")
	}
	l.queue = l.queue[:i]
	return nil
}

// Append 追加 log entry
func (l *memoryLog) Append(entries ...LogEntry) error {
	l.mux.Lock()
	defer l.mux.Unlock()

	return l.append(entries...)
}
func (l *memoryLog) append(entries ...LogEntry) error {
	start, _, err := l.last()
	if err != nil {
		return err
	}

	for i := range entries {
		entries[i].Index = start + uint64(i) + 1
	}
	l.queue = append(l.queue, entries...)
	return nil
}

func (l *memoryLog) AppendAt(prevIndex uint64, entries ...LogEntry) error {
	l.mux.Lock()
	defer l.mux.Unlock()

	err := l.popAfter(prevIndex)
	if err != nil {
		return err
	}
	return l.append(entries...)
}

// AppendEntry append log entry and return log entry index
func (l *memoryLog) AppendEntry(entry LogEntry) (index uint64, err error) {
	l.mux.Lock()
	defer l.mux.Unlock()

	start, _, err := l.last()
	if err != nil {
		return 0, err
	}

	entry.Index = start + 1
	l.queue = append(l.queue, entry)
	return entry.Index, nil
}
