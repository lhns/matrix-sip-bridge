package calls

// NoticeConfig switches the things the bridge says about itself.
//
// Every category is on by default and each is a pointer, because an absent key
// must mean "on" rather than the zero value: the point of these keys is to be
// able to quieten a category that turns out to be noisy without a code change,
// not to have to enable each one.
type NoticeConfig struct {
	// CallEnded posts the timeline record of a finished call.
	CallEnded *bool `yaml:"call_ended"`
	// CallFailed folds a call the bridge could not carry into that record.
	CallFailed *bool `yaml:"call_failed"`
}

func (n NoticeConfig) callEnded() bool  { return onUnlessSet(n.CallEnded) }
func (n NoticeConfig) callFailed() bool { return onUnlessSet(n.CallFailed) }

func onUnlessSet(v *bool) bool { return v == nil || *v }
