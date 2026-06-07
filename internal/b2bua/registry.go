package b2bua

import "sync"

// Registry tracks active calls by call ID and by inbound dialog ID.
// All methods are safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	m        map[string]*Call
	byDialog map[string]*Call
}

func (r *Registry) add(c *Call) {
	r.mu.Lock()
	r.m[c.id] = c
	r.mu.Unlock()
}

func (r *Registry) addDialog(dialogID string, c *Call) {
	if dialogID == "" {
		return
	}
	r.mu.Lock()
	r.byDialog[dialogID] = c
	r.mu.Unlock()
}

func (r *Registry) getByDialog(dialogID string) (*Call, bool) {
	r.mu.Lock()
	c, ok := r.byDialog[dialogID]
	r.mu.Unlock()
	return c, ok
}

// remove deletes the call from both indexes. An empty dialogID is tolerated.
func (r *Registry) remove(id, dialogID string) {
	r.mu.Lock()
	delete(r.m, id)
	if dialogID != "" {
		delete(r.byDialog, dialogID)
	}
	r.mu.Unlock()
}

func (r *Registry) get(id string) (*Call, bool) {
	r.mu.Lock()
	c, ok := r.m[id]
	r.mu.Unlock()
	return c, ok
}

func (r *Registry) len() int {
	r.mu.Lock()
	n := len(r.m)
	r.mu.Unlock()
	return n
}

// ActiveCalls returns the number of live calls. It is the CallSource gauge read
// for sequencer_active_calls.
func (r *Registry) ActiveCalls() int {
	return r.len()
}

// ActiveLegs returns the total live leg count across all calls: one inbound leg
// per call plus each call's app legs and (if present) its PBX leg. It is the
// CallSource gauge read for sequencer_active_legs. The registry lock is dropped
// before reading each call so a slow call lock cannot block scrapes registry-wide.
func (r *Registry) ActiveLegs() int {
	r.mu.Lock()
	calls := make([]*Call, 0, len(r.m))
	for _, c := range r.m {
		calls = append(calls, c)
	}
	r.mu.Unlock()

	n := 0
	for _, c := range calls {
		c.mu.Lock()
		n += 1 + len(c.appLegs)
		if c.pbxLeg != nil {
			n++
		}
		c.mu.Unlock()
	}
	return n
}

// each calls f for every active call. The registry lock is not held during f.
func (r *Registry) each(f func(*Call)) {
	r.mu.Lock()
	calls := make([]*Call, 0, len(r.m))
	for _, c := range r.m {
		calls = append(calls, c)
	}
	r.mu.Unlock()
	for _, c := range calls {
		f(c)
	}
}
