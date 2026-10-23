//go:build integration && !race

package golden

// raceEnabled makes the tools under test build with the race detector too.
const raceEnabled = false
