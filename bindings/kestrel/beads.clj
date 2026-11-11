(ns kestrel.skills.beads
  "bd CLI as Kestrel's durable task memory. Policy per docs/PROCESS.md.
   DRAFT — not yet run against a live bd. See README.md for what to verify."
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
  (let [{:keys [exit out err]} (p/sh {:continue true} (concat ["bd"] args ["--json"]))]
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
   match what `session-start` filters on."
  [{:keys [bops-path actor] :or {bops-path "bops"}} & args]
  (let [opts (cond-> {:continue true}
               actor (assoc :extra-env {"BEADS_ACTOR" actor}))
        {:keys [exit out err]} (p/sh opts (concat [bops-path] args ["--json"]))]
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

(defn sync!
  "bd sync; exit 3 is transient and treated as ok. Never throws — returns a map."
  []
  (let [{:keys [exit out]} (p/sh {:continue true} ["bd" "sync" "--json"])]
    (case exit
      0 {:synced true}
      3 {:synced false :transient true}
      {:synced false :offline true :exit exit :out out})))

(defn session-start
  "Returns {:sync .. :in-progress [...] :ready [...] :resume bead-or-nil}.
   in_progress beats ready. Skips needs-human and foreign-machine beads.
   Throws if the resume target is in_progress with no NEXT in its handoff."
  [{:keys [actor machine-label]}]
  (let [sync-result (sync!)
        _           (try (bd "gate" "check") (catch Exception _ nil))
        in-prog     (bd "list" "--status" "in_progress")
        ready       (bd "ready")
        machine?    (fn [b] (let [ls (set (:labels b))
                                  bound (filter #(re-find #"^(gpu-|mac-|linux-|host-|debian-|arch-)" %) ls)]
                              (or (empty? bound) (contains? ls machine-label))))
        mine        (filter #(= actor (:assignee %)) in-prog)
        eligible    (->> ready
                         (remove #(some #{"needs-human"} (:labels %)))
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
                            [lint-path id "--json" "--no-sync"
                             "--actor" actor "--machine" machine-label])]
    (try (json/parse-string out true)
         (catch Exception _ {:workable false :fails ["lint produced no JSON"]}))))

(defn parse-claim
  "Pure: turn `bops claim --json`'s exit code and parsed document into a result
   map (SPEC §4 ownership outcomes, bops/commands.json `claim`).
     exit 0, outcome claimed|held -> {:outcome :claimed|:held ...}   you own it
     exit 3, outcome lost         -> {:outcome :lost :holder {:actor :run :age_minutes}}
     exit 3, outcome refused      -> {:outcome :refused :reason \"...\"}
     anything else                -> {:outcome :error :code .. :error .. :exit ..}
   :error is a run that could not complete (exit 1, or 2 for usage): `code` is
   the stable code (unreachable, git_auth, lock_unsupported, no_origin,
   remote_mismatch, ...) and :bops-outcome is bops's own outcome (unreachable
   carries :retry and :side-effects). Every map keeps the raw document as :doc."
  [exit doc]
  (let [outcome (:outcome doc)
        base    {:id (:id doc) :run (:run doc) :exit exit :doc doc
                 :warnings (vec (:warnings doc))}]
    (cond
      (and (= 0 exit) (#{"claimed" "held"} outcome))
      (assoc base :outcome (keyword outcome)
             :claimed-at (:claimed_at doc)
             :ledger-pending (boolean (:ledger_pending doc)))

      (and (= 3 exit) (= "lost" outcome))
      (assoc base :outcome :lost :holder (:holder doc))

      (and (= 3 exit) (= "refused" outcome))
      (assoc base :outcome :refused :reason (:reason doc)
             :age-minutes (:age_minutes doc) :stale-minutes (:stale_minutes doc))

      :else
      (assoc base :outcome :error
             :bops-outcome outcome
             :code (:code doc)
             :error (or (:error doc) "bops claim produced no JSON result")
             :retry (:retry doc)
             :side-effects (:side_effects doc)))))

(defn claim!
  "Lint, then `bops claim <id>`, which takes the bead through the claim lock
   (refs/bops/claims/<id>). Returns the `parse-claim` map:
     :claimed / :held  -> you own the bead; work it
     :lost             -> someone else holds it (:holder); pick another bead
     :refused          -> a claim precondition failed (:reason); pick another bead
   Throws ex-info, like `bd` and the lint check, when the bead is not workable
   (\"bead not workable\") or when bops could not run (\"bops claim could not
   run\", with the parse map as data: :code, :error, :exit, :retry,
   :side-effects). Never falls back to `bd update --claim`.
   Releasing, landing and closing still go through raw bd here (see README)."
  [cfg id]
  (let [{:keys [workable fails] :as r} (lint cfg id)]
    (when-not workable
      (throw (ex-info "bead not workable" {:id id :fails fails :lint r})))
    (let [{:keys [exit doc err]} (bops cfg "claim" id)
          result (parse-claim exit doc)]
      (when (= :error (:outcome result))
        (throw (ex-info "bops claim could not run"
                        (assoc result :id id :err err))))
      result)))

(defn handoff! [id handoff]
  (m/assert Handoff handoff)
  (bd "update" id "--append-notes" (render-handoff handoff)))

(defn discovered!
  "File discovered work with provenance. Returns new id."
  [{:keys [title type priority from]}]
  (:id (one (bd "create" title "-t" (or type "task") "-p" (str (or priority 2))
                "--deps" (str "discovered-from:" from)))))

(defn block! [id reason]
  (bd "update" id "--status" "blocked" "--append-notes" (str "BLOCKER: " reason)))

(defn gate-human! [id reason]
  (bd "gate" "create" "--type" "human" "--blocks" id "--reason" reason)
  (bd "label" "add" id "needs-human"))

(defn close!
  "Refuses to close epics (§2.5). Returns the new ready front."
  [{:keys [id issue_type]} reason]
  (when (= "epic" issue_type)
    (throw (ex-info "agents do not close epics" {:id id})))
  (bd "close" id "--reason" reason)
  (bd "ready"))

(comment
  (def cfg {:actor "kestrel@debian-box" :machine-label "debian-box"})
  (session-start cfg)
  (claim! cfg "bd-a1b2")   ; => {:outcome :claimed ...} | :lost | :refused
  (handoff! "bd-a1b2" {:completed "parser" :in-progress "emitter" :next "run make test-emit"})
  )
