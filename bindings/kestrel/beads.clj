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

(defn claim!
  "Lint, then claim. Throws with the lint fails if not workable.
  Uses `bd update --claim`, which bypasses the bops claim lock (PROCESS §1);
  bead bops-ecs moves it to `bops claim`."
  [cfg id]
  (let [{:keys [workable fails] :as r} (lint cfg id)]
    (when-not workable
      (throw (ex-info "bead not workable" {:id id :fails fails :lint r})))
    (bd "update" id "--claim")))

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
  (claim! cfg "bd-a1b2")
  (handoff! "bd-a1b2" {:completed "parser" :in-progress "emitter" :next "run make test-emit"})
  )
