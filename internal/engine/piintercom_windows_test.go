package engine

// The pi-intercom bridge does not support Windows yet, so neither does its
// fake Pi agent.
func fakePiChild([]byte) int { return 1 }
