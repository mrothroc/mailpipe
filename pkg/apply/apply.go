// Package apply carries out a stored plan against the mailbox. Every message
// is looked up on the server immediately before it is acted on, and is moved
// only if it still has the Message-ID, sender and folder it was planned with.
package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mrothroc/mailpipe/pkg/store"
)

// ErrStalePlan reports a plan made before plans recorded each message's folder.
var ErrStalePlan = errors.New("plan does not record the folder each message was planned in")

// WouldMove is the outcome of an item that passed every check in check mode.
const WouldMove = "would_move"

// Remote is a message as the server reports it now.
type Remote struct {
	InternetMessageID string
	Sender            string
	ParentFolderID    string
}

// Found is the result of looking up one message.
type Found struct {
	Remote  Remote
	Missing bool  // the server has no such message
	Err     error // the lookup itself failed
}

// Mailbox is what apply needs from the mail server. Lookup and Move answer
// for each id, in the order given; their own error means the whole call failed.
type Mailbox interface {
	FolderID(ctx context.Context, name string) (string, error)
	Lookup(ctx context.Context, graphIDs []string) ([]Found, error)
	Move(ctx context.Context, graphIDs []string, destination string) ([]error, error)
}

// Log is the action log apply reads and writes.
type Log interface {
	Moved(ctx context.Context, planID int64) (map[int64]bool, error)
	LogActions(ctx context.Context, actions []store.Action) error
}

// Options selects what a run does.
type Options struct {
	Execute     bool   // false checks every item and changes nothing
	MaxItems    int    // at most this many pending items, in plan order; 0 means all
	Action      string // the plan action to carry out
	Destination string // the folder those messages move to
}

// Outcome is what happened to one item.
type Outcome struct {
	Item   store.PlanItem
	Status string // WouldMove, or one of the store's action statuses
	Detail string
}

// Summary counts a run's outcomes.
type Summary struct {
	AlreadyMoved int // moved by an earlier run of this plan, so not touched
	WouldMove    int
	Moved        int
	Skipped      int
	Failed       int
}

func (s *Summary) add(status string) {
	switch status {
	case WouldMove:
		s.WouldMove++
	case store.ActionMoved:
		s.Moved++
	case store.ActionSkipped:
		s.Skipped++
	case store.ActionFailed:
		s.Failed++
	}
}

// Run is the legacy-unbound compatibility API; bound CLI consumers use RunBound.
// Run carries out the plan's items for opts.Action, calling emit for each
// item it examines. With Execute it logs every attempt; a failed or skipped
// item stays pending for the next run. A lookup that fails as a whole stops
// the run before that batch moves anything; a move that fails as a whole, or a
// failed write of the action log, stops it after that batch's outcomes have
// been reported (and, for a failed move, logged as failed).
func Run(ctx context.Context, log Log, box Mailbox, planID int64, items []store.PlanItem, opts Options, emit func(Outcome) error) (Summary, error) {
	var sum Summary
	done, err := log.Moved(ctx, planID)
	if err != nil {
		return sum, err
	}
	var todo []store.PlanItem
	for _, it := range items {
		switch {
		case it.Action != opts.Action:
			continue
		case it.PlannedFolder == "":
			return sum, fmt.Errorf("%w (message %d)", ErrStalePlan, it.N)
		case done[it.N]:
			sum.AlreadyMoved++
			continue
		}
		todo = append(todo, it)
	}
	if opts.MaxItems > 0 && len(todo) > opts.MaxItems {
		todo = todo[:opts.MaxItems]
	}
	folders := map[string]string{}
	for _, it := range todo {
		if _, ok := folders[it.PlannedFolder]; !ok {
			if folders[it.PlannedFolder], err = box.FolderID(ctx, it.PlannedFolder); err != nil {
				return sum, fmt.Errorf("resolve folder %s: %w", it.PlannedFolder, err)
			}
		}
	}
	size := len(todo)
	if adapter, ok := box.(interface{ ChunkSize() int }); ok {
		size = adapter.ChunkSize()
	}
	if len(todo) > 0 && size <= 0 {
		return sum, errors.New("apply: adapter supplied invalid chunk size")
	}
	for start := 0; start < len(todo); start += size {
		chunk := todo[start:min(start+size, len(todo))]
		outcomes, runErr := applyChunk(ctx, box, chunk, folders, opts)
		if opts.Execute && len(outcomes) > 0 {
			rows := make([]store.Action, len(outcomes))
			for i, o := range outcomes {
				rows[i] = store.Action{PlanID: planID, N: o.Item.N, GraphID: o.Item.GraphID, Action: opts.Action,
					Destination: opts.Destination, Status: o.Status, Detail: o.Detail}
			}
			if err := log.LogActions(ctx, rows); err != nil {
				// The moves happened regardless: report them, then stop.
				runErr = errors.Join(runErr, fmt.Errorf("write the action log: %w", err))
			}
		}
		for _, o := range outcomes {
			sum.add(o.Status)
			if err := emit(o); err != nil {
				runErr = errors.Join(runErr, err)
			}
		}
		if runErr != nil {
			return sum, runErr
		}
	}
	return sum, nil
}

// applyChunk checks one batch of items and, with Execute, moves those that
// passed. Its error means a call failed as a whole: a failed lookup returns no
// outcomes (nothing moved); after a failed move the outcomes still say what
// happened to each item.
func applyChunk(ctx context.Context, box Mailbox, chunk []store.PlanItem, folders map[string]string, opts Options) ([]Outcome, error) {
	ids := make([]string, len(chunk))
	for i, it := range chunk {
		ids[i] = it.GraphID
	}
	found, err := box.Lookup(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("check messages: %w", err)
	}
	if len(found) != len(chunk) {
		return nil, fmt.Errorf("check messages: %d answers for %d messages", len(found), len(chunk))
	}
	outcomes := make([]Outcome, len(chunk))
	var ready []int
	for i, it := range chunk {
		outcomes[i] = verify(it, found[i], folders[it.PlannedFolder])
		if outcomes[i].Status == WouldMove {
			ready = append(ready, i)
		}
	}
	if !opts.Execute || len(ready) == 0 {
		return outcomes, nil
	}
	moveIDs := make([]string, len(ready))
	for j, i := range ready {
		moveIDs[j] = chunk[i].GraphID
	}
	errs, err := box.Move(ctx, moveIDs, opts.Destination)
	if err == nil && len(errs) != len(ready) {
		err = fmt.Errorf("%d answers for %d moves", len(errs), len(ready))
	}
	for j, i := range ready {
		switch {
		case err != nil:
			// Some moves may have happened; the next run's check finds those
			// outside the planned folder and skips them.
			outcomes[i].Status, outcomes[i].Detail = store.ActionFailed, "move: "+err.Error()
		case errs[j] != nil:
			outcomes[i].Status, outcomes[i].Detail = store.ActionFailed, "move: "+errs[j].Error()
		default:
			outcomes[i].Status = store.ActionMoved
		}
	}
	if err != nil {
		return outcomes, fmt.Errorf("move messages: %w", err)
	}
	return outcomes, nil
}

func verify(it store.PlanItem, f Found, folderID string) Outcome {
	o := Outcome{Item: it, Status: store.ActionSkipped}
	switch {
	case f.Err != nil:
		o.Status, o.Detail = store.ActionFailed, "check: "+f.Err.Error()
	case f.Missing:
		o.Detail = "no longer in the mailbox"
	case f.Remote.InternetMessageID != it.InternetMessageID:
		o.Detail = "Message-ID changed"
	case !strings.EqualFold(strings.TrimSpace(f.Remote.Sender), strings.TrimSpace(it.Sender)):
		o.Detail = "sender changed"
	case f.Remote.ParentFolderID != folderID:
		o.Detail = "no longer in " + it.PlannedFolder
	default:
		o.Status = WouldMove
	}
	return o
}
