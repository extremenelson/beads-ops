(ns kestrel.skills.beads
  "bd CLI as Kestrel's durable task memory. Policy per docs/PROCESS.md.
   DRAFT — not yet run against a live bd or bops. See README.md for what to
   verify."
  (:require [babashka.process :as p]
            [cheshire.core :as json]
            [clojure.string :as str]
            [malli.core :as m]))

;; --- schemas --------------------------------------------------------------

(def Handoff
  [:map
   [:completed string?]
   [:in-progress string?]
   [:next string?]                      ; hard requirement at resume
   [:blocker {:optional true} string?]
   [:key-decision {:optional true} string?]
   [:ref {:optional true} string?]])

(def Bead
  [:map
   [:id string?]
   [:title string?]
   [:status [:enum "open" "in_progress" "blocked" "closed" "deferred"]]
   [:priority [:int {:min 0 :max 4}]]
   [:issue_type string?]
   [:assignee {:optional true} [:maybe string?]]
   [:notes {:optional true} [:maybe string?]]
   [:labels {:optional true} [:vector string?]]
   [:parent {:optional true} [:maybe string?]]])

;; --- cli ------------------------------------------------------------------

(defn bd
  "Run bd with --json, return parsed EDN. Throws ex-info on non-zero exit,
   carrying bd's own {error code hint} when it emitted JSON on stderr."
  [& args]
  (let [{:keys [exit out err]} (apply p/sh {:continue true} "bd" (concat args ["--json"]))]
    (when-not (zero? exit)
      (throw (ex-info "bd failed"
                      (merge {:args args :exit exit :err err}
                             (try (json/parse-string err true) (catch Exception _ nil))))))
    (json/parse-string out true)))

(defn bops
  "Run bops with --json. Never throws on exit status: returns
   {:exit n :doc parsed-json-or-nil :err stderr}, because exit 3 is a verdict
   (lost, refused), not a failure. `cfg` supplies :bops-path (default \"bops\")
   and :actor, passed as BEADS_ACTOR so the claim ref and the ledger assignee
   match what `session-start` filters on. `:in`, when set, is written to bops's
   stdin (pair it with `--file -`: bops reads stdin only when asked)."
  [{:keys [bops-path actor in] :or {bops-path "bops"}} & args]
  (let [opts (cond-> {:continue true}
               actor (assoc :extra-env {"BEADS_ACTOR" actor})
               in    (assoc :in in))
        {:keys [exit out err]} (apply p/sh opts bops-path (concat args ["--json"]))]
    {:exit exit
     :err  err
     :doc  (try (json/parse-string out true) (catch Exception _ nil))}))

(defn- one [x] (if (sequential? x) (first x) x))

;; --- handoff notes (§4) ---------------------------------------------------

(def ^:private note-re #"(?m)^(COMPLETED|IN PROGRESS|NEXT|BLOCKER|KEY DECISION|REF):\s*(.+)$")

(defn parse-handoff
  "Parse the LAST handoff block in a notes field. nil if none."
  [notes]
  (when (seq notes)
    (let [last-block (last (str/split notes #"\n\s*\n"))
          kvs (->> (re-seq note-re last-block)
                   (map (fn [[_ k v]]
                          [(keyword (str/lower-case (str/replace k " " "-"))) (str/trim v)]))
                   (into {}))]
      (when (seq kvs) kvs))))

(defn render-handoff [{:keys [completed in-progress next blocker key-decision ref]}]
  (->> [(str "COMPLETED: " completed)
        (str "IN PROGRESS: " in-progress)
        (str "NEXT: " next)
        (str "BLOCKER: " (or blocker "none"))
        (when key-decision (str "KEY DECISION: " key-decision))
        (when ref (str "REF: " ref))]
       (remove nil?) (str/join "\n")))

;; --- session protocol (§2) ------------------------------------------------

(defn parse-sync
  "Pure: turn `bops sync --json`'s exit code and parsed document into a result
   map (bops/commands.json `sync`).
     exit 0 -> {:synced true :pulled :pushed :revision :unpublished
                :lost-claims [...] :stranded-holds [...] :warnings [...]}
     else   -> {:synced false :offline true :exit .. :code .. :error ..}
   bops retries a Dolt lock once itself, so there is no transient exit 3 now.
   :lost-claims are beads someone took from this run: stop working them.
   Every map keeps the raw document as :doc."
  [exit doc]
  (if (= 0 exit)
    {:synced true :exit exit :doc doc
     :pulled (boolean (:pulled doc)) :pushed (boolean (:pushed doc))
     :revision (:revision doc) :unpublished (:unpublished doc)
     :lost-claims (vec (:lost_claims doc))
     :stranded-holds (vec (:stranded_holds doc))
     :warnings (vec (:warnings doc))}
    {:synced false :offline true :exit exit :doc doc
     :code (:code doc)
     :error (or (:error doc) "bops sync produced no JSON result")}))

(defn sync!
  "`bops sync`: fetch the claim refs, pull, heartbeat this run's holds, finish
   pending ref deletes, push. Never throws: returns the `parse-sync` map, with
   {:synced false :offline true :code ..} when bops could not run."
  ([] (sync! {}))
  ([cfg]
   (let [{:keys [exit doc err]} (bops cfg "sync")]
     (assoc (parse-sync exit doc) :err err))))

(def ^:private legacy-needs-human
  "The spelling early bops wrote. Read as needs-human; drop with bops v0.2.0."
  "needs:human")

(defn session-start
  "Returns {:sync .. :in-progress [...] :ready [...] :resume bead-or-nil}.
   in_progress beats ready. Skips beads labelled needs-human, or the legacy
   needs:human that early bops wrote (read until bops v0.2.0, SPEC §3.1), and
   foreign-machine beads.
   :sync is `sync!`'s map. Throws if the resume target is in_progress with no
   NEXT in its handoff."
  [{:keys [actor machine-label] :as cfg}]
  (let [sync-result (sync! cfg)
        _           (try (bd "gate" "check") (catch Exception _ nil))
        in-prog     (bd "list" "--status" "in_progress")
        ready       (bd "ready")
        machine?    (fn [b] (let [ls (set (:labels b))
                                  bound (filter #(re-find #"^(gpu-|mac-|linux-|host-|debian-|arch-)" %) ls)]
                              (or (empty? bound) (contains? ls machine-label))))
        mine        (filter #(= actor (:assignee %)) in-prog)
        eligible    (->> ready
                         (remove #(some #{"needs-human" legacy-needs-human} (:labels %)))
                         (filter machine?))
        resume      (or (first (sort-by :priority mine))
                        (first (sort-by :priority eligible)))]
    (when (and resume (= "in_progress" (:status resume)))
      (let [full (one (bd "show" (:id resume)))]
        (when (nil? (:next (parse-handoff (:notes full))))
          (throw (ex-info "in_progress bead has no NEXT in handoff note — resume contract broken"
                          {:id (:id resume)})))))
    {:sync sync-result :in-progress in-prog :ready ready :resume resume}))

(defn lint
  "Run scripts/bd-lint-claim.sh --json. Returns {:workable bool :fails [...] :warns [...]}."
  [{:keys [lint-path actor machine-label] :or {lint-path "scripts/bd-lint-claim.sh"}} id]
  (let [{:keys [out]} (p/sh {:continue true}
                            lint-path id "--json" "--no-sync"
                            "--actor" actor "--machine" machine-label)]
    (try (json/parse-string out true)
         (catch Exception _ {:workable false :fails ["lint produced no JSON"]}))))

(def ^:private positive-outcomes
  "Exit-0 outcomes per ownership command (SPEC §4 ownership outcomes)."
  {"claim" #{"claimed" "held"} "release" #{"released"} "land" #{"landed"}})

(defn parse-outcome
  "Pure: turn an ownership command's (`claim`, `release`, `land`) exit code and
   parsed `--json` document into a result map (SPEC §4 ownership outcomes,
   bops/commands.json).
     exit 0, claimed|held|released|landed -> {:outcome :claimed|:held|:released|:landed ...}
     exit 3, outcome lost    -> {:outcome :lost :holder {:actor :run :age_minutes}}
     exit 3, outcome refused -> {:outcome :refused :reason \"...\"}
     anything else           -> {:outcome :error :code .. :error .. :exit ..}
   A positive outcome that is not the command's own (say `landed` from claim)
   is :error. :reason on a refusal is release's `detail` (release's own
   `reason` is the release reason, kept as :release-reason) or claim's and
   land's `reason`. :error is a run that could not complete (exit 1, or 2 for
   usage): `code` is the stable code (unreachable, git_auth, lock_unsupported,
   no_origin, remote_mismatch, ...) and :bops-outcome is bops's own outcome
   (unreachable carries :retry and :side-effects). Every map keeps the raw
   document as :doc."
  [command exit doc]
  (let [outcome (:outcome doc)
        base    (cond-> {:command command :id (:id doc) :run (:run doc) :exit exit
                         :doc doc :warnings (vec (:warnings doc))}
                  (= "release" command) (assoc :release-reason (:reason doc)))]
    (cond
      (and (= 0 exit) (contains? (positive-outcomes command) outcome))
      (cond-> (assoc base :outcome (keyword outcome)
                     :ledger-pending (boolean (:ledger_pending doc)))
        (= "claim" command)   (assoc :claimed-at (:claimed_at doc))
        (= "release" command) (assoc :ref-pending (boolean (:ref_pending doc)))
        (= "land" command)    (assoc :landed (:landed doc)))

      (and (= 3 exit) (= "lost" outcome))
      (assoc base :outcome :lost :holder (:holder doc))

      (and (= 3 exit) (= "refused" outcome))
      (assoc base :outcome :refused
             :reason (if (= "release" command) (:detail doc) (:reason doc))
             :age-minutes (:age_minutes doc) :stale-minutes (:stale_minutes doc))

      :else
      (assoc base :outcome :error
             :bops-outcome outcome
             :code (:code doc)
             :error (or (:error doc) (str "bops " command " produced no JSON result"))
             :retry (:retry doc)
             :side-effects (:side_effects doc)))))

(defn parse-claim   [exit doc] (parse-outcome "claim" exit doc))
(defn parse-release [exit doc] (parse-outcome "release" exit doc))
(defn parse-land    [exit doc] (parse-outcome "land" exit doc))

(defn- owned!
  "Run an ownership command and parse it. Throws ex-info \"bops <command>
   could not run\" (the parse map plus :id and :err as data) on :error;
   otherwise returns the parse map, :lost and :refused included."
  [cfg command id & args]
  (let [{:keys [exit doc err]} (apply bops cfg command id args)
        result (parse-outcome command exit doc)]
    (when (= :error (:outcome result))
      (throw (ex-info (str "bops " command " could not run")
                      (assoc result :id id :err err))))
    result))

(defn claim!
  "Lint, then `bops claim <id>`, which takes the bead through the claim lock
   (refs/bops/claims/<id>). Returns the `parse-claim` map:
     :claimed / :held  -> you own the bead; work it
     :lost             -> someone else holds it (:holder); pick another bead
     :refused          -> a claim precondition failed (:reason); pick another bead
   Throws ex-info, like `bd` and the lint check, when the bead is not workable
   (\"bead not workable\") or when bops could not run (\"bops claim could not
   run\", with the parse map as data: :code, :error, :exit, :retry,
   :side-effects). Never falls back to `bd update --claim`."
  [cfg id]
  (let [{:keys [workable fails] :as r} (lint cfg id)]
    (when-not workable
      (throw (ex-info "bead not workable" {:id id :fails fails :lint r})))
    (owned! cfg "claim" id)))

(defn handoff! [id handoff]
  (m/assert Handoff handoff)
  (bd "update" id "--append-notes" (render-handoff handoff)))

(defn discovered!
  "File discovered work with provenance. Returns new id."
  [{:keys [title type priority from]}]
  (:id (one (bd "create" title "-t" (or type "task") "-p" (str (or priority 2))
                "--deps" (str "discovered-from:" from)))))

(defn block!
  "`bops release <id> --reason blocked --file -` with \"BLOCKER: <reason>\" on
   stdin: deletes the claim ref, drops this run's hold, clears the assignee,
   sets status blocked and records the BLOCKER text below the event line of
   the release's bops:released comment, all in one write. Returns the
   `parse-release` map:
     :released -> given back, BLOCKER text recorded
     :lost     -> someone else holds it (:holder); nothing is written
     :refused  -> this run does not hold it (:reason); nothing is written
   Throws ex-info \"block! needs a reason\" before running bops when `reason`
   is blank, and \"bops release could not run\" (:code, :error, :exit, :retry,
   :side-effects) on exit 1 or 2."
  [cfg id reason]
  (when (str/blank? reason)
    (throw (ex-info "block! needs a reason" {:id id})))
  (owned! (assoc cfg :in (str "BLOCKER: " reason))
          "release" id "--reason" "blocked" "--file" "-"))

(defn gate-human!
  "`bops release <id> --reason human`: deletes the claim ref, drops this run's
   hold, clears the assignee, reopens the bead and labels it needs-human.
   Returns the `parse-release` map. Only on :released does it then, with raw
   bd, create the human gate (`bd gate create --type human`; bops has no gate
   command in contract 1). :lost and :refused write nothing.
   Throws ex-info \"bops release could not run\" on exit 1 or 2, and bd's
   ex-info if the gate write fails after the release."
  [cfg id reason]
  (let [result (owned! cfg "release" id "--reason" "human")]
    (when (= :released (:outcome result))
      (bd "gate" "create" "--type" "human" "--blocks" id "--reason" reason))
    result))

(defn close!
  "Finish a bead: `bops land <id> --ref <url> --sha <sha>` records the PR/MR as
   evidence, then `bops release <id> --reason done` gives the claim back.
   close! does NOT close the bead: SPEC §2 says only `bops sweep` or a human
   closes, and sweep closes it once the PR is merged. After a :released result
   this run holds nothing; the claim ref stays as state=landing (SPEC §3.0),
   the bead stays in_progress with label bops:landing, and `bops sweep`
   deletes the ref and closes the bead after the merge.
   Refuses epics (§2.5) before any write. `opts` is {:ref url :sha commit
   :verify bool} (:verify passes --verify, checking the sha on the forge).
   Returns the `parse-release` map with :land (the `parse-land` map) added, or,
   when land comes back :lost or :refused, that land map: release is then not
   run. Throws ex-info \"bops land could not run\" or \"bops release could not
   run\" (:code, :error, :exit, ...) on exit 1 or 2; a release failure after a
   landing leaves the evidence recorded; while this run still holds the bead,
   rerunning close! with the same ref and sha lands again without writing
   (bops treats a repeat landing as a no-op) and retries the release."
  [cfg {:keys [id issue_type]} {:keys [ref sha verify]}]
  (when (= "epic" issue_type)
    (throw (ex-info "agents do not close epics" {:id id})))
  (when-not (and (seq ref) (seq sha))
    (throw (ex-info "close! needs :ref (PR/MR URL) and :sha" {:id id})))
  (let [land (apply owned! cfg "land" id "--ref" ref "--sha" sha
                    (when verify ["--verify"]))]
    (if (= :landed (:outcome land))
      (assoc (owned! cfg "release" id "--reason" "done") :land land)
      land)))

(comment
  (def cfg {:actor "kestrel@debian-box" :machine-label "debian-box"})
  (session-start cfg)
  (claim! cfg "bd-a1b2")   ; => {:outcome :claimed ...} | :lost | :refused
  (handoff! "bd-a1b2" {:completed "parser" :in-progress "emitter" :next "run make test-emit"})
  (close! cfg {:id "bd-a1b2" :issue_type "task"}
          {:ref "https://github.com/o/r/pull/7" :sha "0123abc"})   ; => {:outcome :released :land {...}}
  (block! cfg "bd-a1b2" "waiting on vendor API key")             ; => {:outcome :released ...}
  )
