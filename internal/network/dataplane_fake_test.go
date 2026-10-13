package network

// failNext makes the next n calls return err and leave the record unchanged.
func (f *fakeDataplane) failNext(n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails = n
	f.failErr = err
}

func (f *fakeDataplane) has(object string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[object]
	return ok
}

func (f *fakeDataplane) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}
