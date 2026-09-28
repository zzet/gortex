package indexer

import ()

// The coordinator with working-tree chaining on: which parent a cycle builds
// over, what a reader pinned before a flip keeps seeing, what a torn or
// superseded build publishes, how a chain is retired once the route leaves
// it, and the foreground-cost rules the cycle keeps (a refresh ticket skips
// the quiet window, one cycle shares one working-copy sample, no cycle
// deletes payload).

const chainIslandTwo = "package fixture\n\nfunc Island() {\n}\n\nfunc IslandTwo() {\n}\n"
