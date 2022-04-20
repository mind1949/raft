package raft

import (
	logger "log"
	"sort"
	"sync"
	"time"
)

var _ server = (*leader)(nil)

// leader 实现一致性模型在 Leader 状态下的行为
type leader struct {
	*raft

	//	for each server, index of the next log entry
	//	to send to that server (initialized to leader
	//	last log index + 1)
	nextIndex raftIdIndexMap
	//	for each server, index of highest log entry
	//	known to be replicated on server (initialized to 0,
	//	increases monotonically)
	matchIndex raftIdIndexMap

	once sync.Once
}

func (l *leader) Run() (server, error) {
	// Upon election: send initial empty AppendEntries RPC
	// (heartbeat) to each server
	timeout := l.HeartbeatTimout() / 2
	_ = l.callAppendEntriesWithAll(timeout)

	for {
		select {
		case <-l.Done():
			return nil, ErrStopped
		case <-l.ticker.C:
			// repeat during idle periods to
			// prevent election timeouts (§5.2)
			timeout := l.HeartbeatTimout() / 2
			_ = l.callAppendEntriesWithAll(timeout)
		}
	}
}

// Commit
// 在 timeout 时间内完成提交客户端命令 cmd, 否则返回 ErrCommitTimeout
func (l *leader) Commit(timeout time.Duration, cmd ...Command) error {
	// If command received from client: append entry to local log,
	// respond after entry applied to state machine (§5.3)
	entries := make([]LogEntry, len(cmd))
	currentTerm := l.GetCurrentTerm()
	for i := range cmd {
		entries = append(entries, LogEntry{
			Term:    currentTerm,
			Command: cmd[i],
		})
	}
	err := l.Append(entries...)
	if err != nil {
		return err
	}

	return l.callAppendEntriesWithAll(timeout)
}

// ResetTimer
// 重置计时器(心跳)
func (l *leader) ResetTimer() {
	// leader 状态只需要重置一次定时器
	// 接受到其他节点的请求, 无需重置定时器
	l.once.Do(func() {
		timeout := l.HeartbeatTimout()
		l.ticker.Reset(timeout)
	})
}

func (*leader) String() string {
	return "Leader"
}

// callAppendEntriesWithAll
//
// 若没有在 timeout 时间内完成复制到大部分的 follower, 返回 ErrCommitTimeout
func (l *leader) callAppendEntriesWithAll(timeout time.Duration) error {
	var done = make(chan struct{})
	time.AfterFunc(timeout, func() { close(done) })

	replicateCh := make(chan struct{}, len(l.peers))
	go func() {
		defer close(replicateCh)

		var wg sync.WaitGroup
		for id, addr := range l.peers {
			if l.Id().Equal(&id) {
				replicateCh <- struct{}{}
				continue
			}

			wg.Add(1)
			go func(id RaftId, addr RaftAddr) {
				defer wg.Done()

				for {
					select {
					case <-done:
						return
					default:
						// no-op
					}

					nextIndex, _ := l.nextIndex.Load(id)
					prevLogIndex := nextIndex - 1
					prevLogEntry, err := l.Get(prevLogIndex)
					if err != nil && err != ErrLogEntryNotExists {
						return
					}
					prevLogTerm := prevLogEntry.Term

					var entries []LogEntry
					// 为了避免 Figure 8 的问题
					// 若最新 log entry 的 term 不是 currentTerm
					// 则不复制
					lastLogIndex, lastLogTerm := l.Last()
					if lastLogTerm == l.GetCurrentTerm() {
						// FIXME: 什么时候会出现 last log index < next ?
						// If last log index ≥ nextIndex for a follower: send
						// AppendEntries RPC with log entries starting at nextIndex
						if lastLogIndex >= nextIndex {
							entries, err = l.RangeGet(nextIndex-1, lastLogIndex)
							if err != nil {
								logger.Println(err)
								return
							}
						}
					}

					args := AppendEntriesArgs{
						Term:         l.GetCurrentTerm(),
						LeaderId:     l.Id(),
						PrevLogIndex: prevLogIndex,
						PrevLogTerm:  prevLogTerm,
						Entries:      entries,
						LeaderCommit: l.GetCommitIndex(),
					}

					results, err := l.client.CallAppendEntries(addr, args)
					if err != nil {
						logger.Printf("call append entries, addr: %q, err: %+v", addr, err)
						return
					}

					// If successful: update nextIndex and matchIndex for
					// follower (§5.3)
					if results.Success {
						if len(args.Entries) > 0 {
							nextIndex := args.Entries[len(args.Entries)-1].Index + 1
							l.nextIndex.Store(id, nextIndex)
						}
						l.matchIndex.Store(id, prevLogIndex)
						replicateCh <- struct{}{}
						l.calcCommitIndex()
						return
					}
					// If AppendEntries fails because of log inconsistency:
					// decrement nextIndex and retry (§5.3)
					l.nextIndex.Store(id, nextIndex-1)
				}
			}(id, addr)
		}
		wg.Wait()
	}()

	var count int
	for {
		select {
		case <-done:
			return ErrCommitTimeout
		case <-replicateCh:
			count++
			if count > len(l.peers)/2 {
				return nil
			}
		}
	}
}

// calcCommitIndex
func (l *leader) calcCommitIndex() error {
	// Raft never commits log entries from previous terms by count-
	// ing replicas. Only log entries from the leader’s current
	// term are committed by counting replicas; once an entry
	// from the current term has been committed in this way,
	// then all prior entries are committed indirectly because
	// of the Log Matching Property. There are some situations
	// where a leader could safely conclude that an older log en-
	// try is committed (for example, if that entry is stored on ev-
	// ery server), but Raft takes a more conservative approach
	// for simplicity
	_, lastLogTerm := l.Last()
	if lastLogTerm != l.GetCurrentTerm() {
		return nil
	}

	// If there exists an N such that N > commitIndex, a majority
	// of matchIndex[i] ≥ N, and log[N].term == currentTerm:
	// set commitIndex = N (§5.3, §5.4).
	matchIndex := make([]int, 0)
	l.matchIndex.Range(func(_ RaftId, index int) bool {
		matchIndex = append(matchIndex, index)
		return true
	})
	commitIndex := l.GetCommitIndex()
	matchIndex = append(matchIndex, commitIndex)

	sort.Ints(matchIndex)
	mid := len(matchIndex) / 2
	nextCommitIndex := matchIndex[mid]

	if nextCommitIndex <= commitIndex {
		return nil
	}
	entry, err := l.Get(nextCommitIndex)
	if err != nil {
		return err
	}
	if entry.Term != l.GetCurrentTerm() {
		return nil
	}
	l.SetCommitIndex(nextCommitIndex)

	// if commitIndex > lastApplied: increment lastApplied, apply
	// log[lastApplied] to state machine (§5.3)
	l.applyCond.Signal()

	return nil
}

type raftIdIndexMap struct {
	m sync.Map
}

func (m *raftIdIndexMap) Load(id RaftId) (index int, ok bool) {
	value, ok := m.m.Load(id)
	if !ok {
		return 0, false
	}
	return value.(int), true
}

func (m *raftIdIndexMap) Store(id RaftId, index int) {
	m.m.Store(id, index)
}

func (m *raftIdIndexMap) Range(fn func(id RaftId, index int) bool) {
	m.m.Range(func(key, value any) bool {
		id := key.(RaftId)
		index := value.(int)
		return fn(id, index)
	})
}