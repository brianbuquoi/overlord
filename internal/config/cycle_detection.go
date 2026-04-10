package config

import (
	"fmt"
	"strings"
)

// detectCycles checks a pipeline for routing cycles that have no exit path
// (no route to "done" or "dead-letter"). A cycle where every stage has at
// least one path to a terminal is a valid retry-loop pattern and is NOT
// flagged. Only closed cycles with no escape route are errors — these would
// cause infinite loops at runtime.
//
// The algorithm:
//  1. Build an adjacency list of all possible routing targets.
//  2. Compute which stages can reach a terminal ("done", "dead-letter", or
//     a stage with no outgoing edges that isn't part of a cycle).
//  3. Find strongly connected components (SCCs) using Tarjan's algorithm.
//  4. Any SCC of size >= 2 (or a self-loop) where NO member can reach a
//     terminal is a closed cycle and produces an error.
func detectCycles(p Pipeline) error {
	// Build adjacency: stage ID → list of target stage IDs (excluding terminals).
	adj := make(map[string][]string, len(p.Stages))
	// allTargets includes terminals for reachability analysis.
	allTargets := make(map[string][]string, len(p.Stages))
	stageSet := make(map[string]bool, len(p.Stages))

	for _, s := range p.Stages {
		stageSet[s.ID] = true
	}

	for _, s := range p.Stages {
		var targets []string
		var allTgts []string

		addTarget := func(t string) {
			if t == "" {
				return
			}
			allTgts = append(allTgts, t)
			if t != "done" && t != "dead-letter" && stageSet[t] {
				targets = append(targets, t)
			}
		}

		addTarget(s.OnFailure)

		if s.OnSuccess.IsConditional {
			addTarget(s.OnSuccess.Default)
			for _, route := range s.OnSuccess.Routes {
				addTarget(route.Stage)
			}
		} else {
			addTarget(s.OnSuccess.Static)
		}

		// Deduplicate.
		adj[s.ID] = dedup(targets)
		allTargets[s.ID] = dedup(allTgts)
	}

	// Compute which stages can reach a terminal (done or dead-letter).
	canExit := make(map[string]bool, len(p.Stages))
	for _, s := range p.Stages {
		for _, t := range allTargets[s.ID] {
			if t == "done" || t == "dead-letter" {
				canExit[s.ID] = true
				break
			}
		}
	}
	// Propagate: if a stage routes to a stage that can exit, it can exit too.
	// Fixed-point iteration.
	changed := true
	for changed {
		changed = false
		for _, s := range p.Stages {
			if canExit[s.ID] {
				continue
			}
			for _, t := range adj[s.ID] {
				if canExit[t] {
					canExit[s.ID] = true
					changed = true
					break
				}
			}
		}
	}

	// Find cycles using DFS. Only report cycles where NO member can exit.
	for _, s := range p.Stages {
		visited := make(map[string]bool)
		path := []string{s.ID}
		visited[s.ID] = true

		if cycle := findCycle(s.ID, adj, visited, path); cycle != nil {
			// Check if any member of the cycle can exit.
			hasExit := false
			for _, node := range cycle[:len(cycle)-1] { // last element repeats the first
				if canExit[node] {
					hasExit = true
					break
				}
			}
			if !hasExit {
				return fmt.Errorf("pipeline %q contains a routing cycle: %s",
					p.Name, strings.Join(cycle, " \u2192 "))
			}
		}
	}

	return nil
}

// findCycle performs DFS from current, returning the cycle path if found.
func findCycle(current string, adj map[string][]string, visited map[string]bool, path []string) []string {
	for _, next := range adj[current] {
		if visited[next] {
			for i, node := range path {
				if node == next {
					cycle := append([]string{}, path[i:]...)
					cycle = append(cycle, next)
					return cycle
				}
			}
		}

		visited[next] = true
		path = append(path, next)
		if cycle := findCycle(next, adj, visited, path); cycle != nil {
			return cycle
		}
		path = path[:len(path)-1]
		delete(visited, next)
	}
	return nil
}

func dedup(s []string) []string {
	seen := make(map[string]bool, len(s))
	result := s[:0]
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	return result
}
