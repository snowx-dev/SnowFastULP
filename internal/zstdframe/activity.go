package zstdframe

// Activity reports indexing subphases for progress UI (optional).
type Activity struct {
	FrameScan func(start bool)
	Decode    func(start bool)
}

func (a *Activity) beginFrameScan() {
	if a != nil && a.FrameScan != nil {
		a.FrameScan(true)
	}
}

func (a *Activity) endFrameScan() {
	if a != nil && a.FrameScan != nil {
		a.FrameScan(false)
	}
}

func (a *Activity) beginDecode() {
	if a != nil && a.Decode != nil {
		a.Decode(true)
	}
}

func (a *Activity) endDecode() {
	if a != nil && a.Decode != nil {
		a.Decode(false)
	}
}
