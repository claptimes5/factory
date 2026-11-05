------------------------------ MODULE Breaker ------------------------------
(***************************************************************************)
(* Auto-merge's breaker against PRs being set to merge themselves.         *)
(*                                                                         *)
(* Property: once a repo's breaker trips, the factory never turns          *)
(* auto-merge on there again, and every PR it had set to merge itself is   *)
(* turned off, is still being turned off, or you were told to do it.       *)
(*                                                                         *)
(* Each action names the Go it stands for, at the sync commit in           *)
(* SKILL.md. One repo; a workspace's repos share the breaker the same way. *)
(***************************************************************************)
EXTENDS Naturals

CONSTANTS
    PRs,                  \* the PRs the factory may set to merge themselves
    MaxCrashes,           \* how many times the daemon may stop mid-step
    RecordChecksBreaker   \* FALSE models deleting guard.go:902-904

VARIABLES
    tripped,          \* auto_merge_guard.tripped_at != '' (store/guard.go)
    lock,             \* who holds the repo's guardLock: "none", "breaker" or a PR
    rec,              \* the factory's record per PR: autoMerged (stages.go:1287)
    forge,            \* the forge's state per PR: "off", "on" or "merged"
    alerted,          \* PRs you were told to turn off yourself
    todo,             \* PRs checkBreaker has yet to hand back
    bpc,              \* where checkBreaker is
    spc,              \* where each PR's start is
    crashes,
    enabledAfterTrip  \* history: auto-merge turned on while tripped

vars == <<tripped, lock, rec, forge, alerted, todo, bpc, spc, crashes, enabledAfterTrip>>

Init ==
    /\ tripped = FALSE
    /\ lock = "none"
    /\ rec = [p \in PRs |-> "none"]
    /\ forge = [p \in PRs |-> "off"]
    /\ alerted = {}
    /\ todo = {}
    /\ bpc = "idle"
    /\ spc = [p \in PRs |-> "start"]
    /\ crashes = 0
    /\ enabledAfterTrip = FALSE

(***************************************************************************)
(* A PR's start: startMerge, enableAt or resync calling recordMergeOn      *)
(* (guard.go:898-914), and autoMerge's cleanup when it fails               *)
(* (stages.go:1259-1263).                                                  *)
(***************************************************************************)

\* recordMergeOn takes the repo's guard lock (guard.go:899-901).
SLock(p) ==
    /\ spc[p] = "start" /\ lock = "none"
    /\ lock' = p
    /\ spc' = [spc EXCEPT ![p] = "locked"]
    /\ UNCHANGED <<tripped, rec, forge, alerted, todo, bpc, crashes, enabledAfterTrip>>

\* It refuses once the breaker tripped (guard.go:902-904). autoMerge then
\* cancels, which does nothing with no record on (stages.go:1313-1315).
SRefused(p) ==
    /\ spc[p] = "locked" /\ RecordChecksBreaker /\ tripped
    /\ lock' = "none"
    /\ spc' = [spc EXCEPT ![p] = "done"]
    /\ UNCHANGED <<tripped, rec, forge, alerted, todo, bpc, crashes, enabledAfterTrip>>

\* It records auto-merge on before asking the forge (guard.go:905-909).
SRecord(p) ==
    /\ spc[p] = "locked" /\ (~RecordChecksBreaker \/ ~tripped)
    /\ rec' = [rec EXCEPT ![p] = "on"]
    /\ spc' = [spc EXCEPT ![p] = "recorded"]
    /\ UNCHANGED <<tripped, lock, forge, alerted, todo, bpc, crashes, enabledAfterTrip>>

\* The forge turns it on, still under the lock (guard.go:913).
SEnable(p) ==
    /\ spc[p] = "recorded"
    /\ forge' = [forge EXCEPT ![p] = "on"]
    /\ enabledAfterTrip' = (enabledAfterTrip \/ tripped)
    /\ lock' = "none"
    /\ spc' = [spc EXCEPT ![p] = "done"]
    /\ UNCHANGED <<tripped, rec, alerted, todo, bpc, crashes>>

\* Or the forge call fails, perhaps having taken effect anyway (a timeout:
\* stages.go:1261); autoMerge cancels it after the lock is released.
SEnableFails(p) ==
    /\ spc[p] = "recorded"
    /\ \E f \in {"off", "on"}:
         /\ forge' = [forge EXCEPT ![p] = f]
         /\ enabledAfterTrip' = (enabledAfterTrip \/ (tripped /\ f = "on"))
    /\ lock' = "none"
    /\ spc' = [spc EXCEPT ![p] = "cancelling"]
    /\ UNCHANGED <<tripped, rec, alerted, todo, bpc, crashes>>

\* cancelAutoMerge: CancelAutoMerge succeeds and the record says off
\* (stages.go:1321-1326); nothing to do if the record is already off.
SCancel(p) ==
    /\ spc[p] = "cancelling"
    /\ IF rec[p] = "on"
       THEN /\ forge' = [forge EXCEPT ![p] = IF @ = "on" THEN "off" ELSE @]
            /\ rec' = [rec EXCEPT ![p] = "off"]
       ELSE UNCHANGED <<forge, rec>>
    /\ spc' = [spc EXCEPT ![p] = "done"]
    /\ UNCHANGED <<tripped, lock, alerted, todo, bpc, crashes, enabledAfterTrip>>

\* Or CancelAutoMerge fails, and you're told to turn it off (stages.go:1321-1324).
SCancelFails(p) ==
    /\ spc[p] = "cancelling" /\ rec[p] = "on"
    /\ alerted' = alerted \cup {p}
    /\ spc' = [spc EXCEPT ![p] = "done"]
    /\ UNCHANGED <<tripped, lock, rec, forge, todo, bpc, crashes, enabledAfterTrip>>

(***************************************************************************)
(* The breaker: checkBreaker (guard.go:520-570) and handBackTripped       *)
(* (guard.go:596-632), on each poll of main's health (guard.go:360, 460)  *)
(* once strikes reach the threshold.                                       *)
(***************************************************************************)

\* It takes the same lock (guard.go:532-534).
BLock ==
    /\ bpc = "idle" /\ lock = "none"
    /\ lock' = "breaker"
    /\ bpc' = "locked"
    /\ UNCHANGED <<tripped, rec, forge, alerted, todo, spc, crashes, enabledAfterTrip>>

\* Already tripped: it returns (guard.go:536-540).
BAlready ==
    /\ bpc = "locked" /\ tripped
    /\ lock' = "none"
    /\ bpc' = "done"
    /\ UNCHANGED <<tripped, rec, forge, alerted, todo, spc, crashes, enabledAfterTrip>>

\* It trips the breaker (guard.go:562) and lists the PRs set to merge
\* themselves (guard.go:599-605).
BTrip ==
    /\ bpc = "locked" /\ ~tripped
    /\ tripped' = TRUE
    /\ todo' = {p \in PRs : rec[p] = "on"}
    /\ bpc' = "loop"
    /\ UNCHANGED <<lock, rec, forge, alerted, spc, crashes, enabledAfterTrip>>

\* handBack cancels one (guard.go:607-611, 872-875; stages.go:1312-1329)...
BHandBack(p) ==
    /\ bpc = "loop" /\ p \in todo
    /\ forge' = [forge EXCEPT ![p] = IF @ = "on" THEN "off" ELSE @]
    /\ rec' = [rec EXCEPT ![p] = "off"]
    /\ todo' = todo \ {p}
    /\ UNCHANGED <<tripped, lock, alerted, bpc, spc, crashes, enabledAfterTrip>>

\* ...or the cancel fails and you're told at once (stages.go:1321-1324).
BHandBackFails(p) ==
    /\ bpc = "loop" /\ p \in todo
    /\ alerted' = alerted \cup {p}
    /\ todo' = todo \ {p}
    /\ UNCHANGED <<tripped, lock, rec, forge, bpc, spc, crashes, enabledAfterTrip>>

\* The loop ends; the lock is released (guard.go:534, deferred).
BDone ==
    /\ bpc = "loop" /\ todo = {}
    /\ lock' = "none"
    /\ bpc' = "done"
    /\ UNCHANGED <<tripped, rec, forge, alerted, todo, spc, crashes, enabledAfterTrip>>

\* The next poll checks again.
BAgain ==
    /\ bpc = "done"
    /\ bpc' = "idle"
    /\ UNCHANGED <<tripped, lock, rec, forge, alerted, todo, spc, crashes, enabledAfterTrip>>

(***************************************************************************)
(* The world.                                                              *)
(***************************************************************************)

\* The forge merges a PR set to merge itself, whenever its checks pass.
Merge(p) ==
    /\ forge[p] = "on"
    /\ forge' = [forge EXCEPT ![p] = "merged"]
    /\ UNCHANGED <<tripped, lock, rec, alerted, todo, bpc, spc, crashes, enabledAfterTrip>>

\* The daemon stops between two steps and starts again. The store and the
\* forge keep what they had; the in-memory lock and loop are gone. Starts
\* that were under way aren't retried. Before anything else runs, a
\* tripped breaker's hand-backs are finished (finishTrips, guard.go:576-591,
\* from Recover): the breaker's loop again, over what's on record as on.
Crash ==
    /\ crashes < MaxCrashes
    /\ crashes' = crashes + 1
    /\ lock' = IF tripped THEN "breaker" ELSE "none"
    /\ todo' = IF tripped THEN {p \in PRs : rec[p] = "on"} ELSE {}
    /\ bpc' = IF tripped THEN "loop" ELSE "idle"
    /\ spc' = [p \in PRs |-> IF spc[p] = "start" THEN "start" ELSE "done"]
    /\ UNCHANGED <<tripped, rec, forge, alerted, enabledAfterTrip>>

Next ==
    \/ \E p \in PRs : SLock(p) \/ SRefused(p) \/ SRecord(p) \/ SEnable(p)
                      \/ SEnableFails(p) \/ SCancel(p) \/ SCancelFails(p)
                      \/ BHandBack(p) \/ BHandBackFails(p) \/ Merge(p)
    \/ BLock \/ BAlready \/ BTrip \/ BDone \/ BAgain
    \/ Crash

Spec == Init /\ [][Next]_vars

(***************************************************************************)
(* The property.                                                           *)
(***************************************************************************)

\* Something under way will still settle the PR: the breaker's loop has it
\* listed, or its own start is mid-way or cancelling.
Settling(p) ==
    \/ bpc = "loop" /\ p \in todo
    \/ spc[p] \in {"recorded", "cancelling"}

\* Once tripped, no PR is left set to merge itself with nothing under way
\* to turn it off and nobody told.
NothingLeftMerging ==
    tripped => \A p \in PRs : forge[p] = "on" => (p \in alerted \/ Settling(p))

\* Auto-merge is never turned on after the breaker tripped.
NeverOnAfterTrip == ~enabledAfterTrip
=============================================================================
