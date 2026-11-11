;; Smoke test for beads.clj: the namespace loads, and parse-claim maps the
;; `bops claim --json` shapes (bops/commands.json `claim`, SPEC §4) to results.
;; Needs no bd or bops. Run from the repo root:
;;   bb -Sdeps '{:deps {metosin/malli {:mvn/version "0.16.1"}}}' bindings/kestrel/beads_test.clj
(require '[clojure.test :refer [deftest is run-tests]]
         '[babashka.process :as p]
         '[cheshire.core :as json])

(load-file "bindings/kestrel/beads.clj")
(alias 'b 'kestrel.skills.beads)

(defn- parse [exit s] (b/parse-claim exit (json/parse-string s true)))

(deftest claimed
  (let [r (parse 0 "{\"contract\":1,\"command\":\"claim\",\"ok\":true,\"id\":\"bd-a1\",\"run\":\"01RUN\",
                     \"outcome\":\"claimed\",\"claimed_at\":\"2026-11-11T10:00:00Z\",\"readback_ok\":true,
                     \"ledger_pending\":true,\"warnings\":[]}")]
    (is (= :claimed (:outcome r)))
    (is (= "bd-a1" (:id r)))
    (is (true? (:ledger-pending r)))))

(deftest held
  (is (= :held (:outcome (parse 0 "{\"contract\":1,\"command\":\"claim\",\"ok\":true,\"id\":\"bd-a1\",\"run\":\"01RUN\",\"outcome\":\"held\",\"warnings\":[]}")))))

(deftest lost
  (let [r (parse 3 "{\"contract\":1,\"command\":\"claim\",\"ok\":false,\"id\":\"bd-a1\",\"run\":\"01RUN\",
                     \"outcome\":\"lost\",\"holder\":{\"actor\":\"alex\",\"run\":\"01OTHER\",\"age_minutes\":7},\"warnings\":[]}")]
    (is (= :lost (:outcome r)))
    (is (= {:actor "alex" :run "01OTHER" :age_minutes 7} (:holder r)))))

(deftest refused
  (let [r (parse 3 "{\"contract\":1,\"command\":\"claim\",\"ok\":false,\"id\":\"bd-a1\",\"run\":\"01RUN\",
                     \"outcome\":\"refused\",\"reason\":\"status is closed\",\"warnings\":[]}")]
    (is (= :refused (:outcome r)))
    (is (= "status is closed" (:reason r)))))

(deftest could-not-run
  (let [r (parse 1 "{\"contract\":1,\"command\":\"claim\",\"ok\":false,\"error\":\"lock remote origin is not the Dolt remote\",
                     \"code\":\"remote_mismatch\",\"id\":\"bd-a1\",\"run\":\"01RUN\",\"outcome\":\"error\",\"warnings\":[]}")]
    (is (= :error (:outcome r)))
    (is (= "remote_mismatch" (:code r)))
    (is (= "lock remote origin is not the Dolt remote" (:error r))))
  (let [r (parse 1 "{\"contract\":1,\"command\":\"claim\",\"ok\":false,\"error\":\"remote not reached\",\"code\":\"unreachable\",
                     \"id\":\"bd-a1\",\"run\":\"01RUN\",\"outcome\":\"unreachable\",\"retry\":\"r\",\"side_effects\":\"s\"}")]
    (is (= [:error "unreachable" "unreachable" "s"] ((juxt :outcome :bops-outcome :code :side-effects) r))))
  (let [r (b/parse-claim 2 nil)]                     ; usage error, no JSON on stdout
    (is (= :error (:outcome r)))
    (is (string? (:error r)))))

(deftest claim-throws-when-bops-cannot-run
  (with-redefs [b/lint (fn [_ _] {:workable true})
                b/bops (fn [_ & _] {:exit 1 :err "" :doc {:ok false :code "no_origin" :error "no remote" :outcome "error"}})]
    (let [e (try (b/claim! {} "bd-a1") nil (catch clojure.lang.ExceptionInfo e e))]
      (is (some? e))
      (is (= "no_origin" (:code (ex-data e))))))
  (with-redefs [b/lint (fn [_ _] {:workable true})
                b/bops (fn [_ & _] {:exit 3 :err "" :doc {:ok false :outcome "lost" :holder {:actor "x"}}})]
    (is (= :lost (:outcome (b/claim! {} "bd-a1"))))))

;; --- release / land / sync parsers ---------------------------------------

(defn- js [s] (json/parse-string s true))

(deftest release-parse
  (let [r (b/parse-release 0 (js "{\"contract\":1,\"command\":\"release\",\"ok\":true,\"id\":\"bd-a1\",\"reason\":\"blocked\",
                                   \"outcome\":\"released\",\"ref_pending\":true,\"warnings\":[]}"))]
    (is (= :released (:outcome r)))
    (is (= "blocked" (:release-reason r)))
    (is (true? (:ref-pending r)))
    (is (false? (:ledger-pending r))))
  (let [r (b/parse-release 3 (js "{\"ok\":false,\"id\":\"bd-a1\",\"reason\":\"done\",\"outcome\":\"lost\",
                                   \"holder\":{\"actor\":\"alex\",\"run\":\"01OTHER\",\"age_minutes\":3}}"))]
    (is (= :lost (:outcome r)))
    (is (= "alex" (get-in r [:holder :actor]))))
  (let [r (b/parse-release 3 (js "{\"ok\":false,\"id\":\"bd-a1\",\"reason\":\"human\",\"outcome\":\"refused\",
                                   \"detail\":\"this run does not hold bd-a1\"}"))]
    (is (= :refused (:outcome r)))
    (is (= "this run does not hold bd-a1" (:reason r)) "refusal reason comes from detail")
    (is (= "human" (:release-reason r))))
  (let [r (b/parse-release 1 (js "{\"ok\":false,\"id\":\"bd-a1\",\"reason\":\"done\",\"outcome\":\"unreachable\",
                                   \"code\":\"unreachable\",\"error\":\"remote not reached\"}"))]
    (is (= [:error "unreachable" "unreachable"] ((juxt :outcome :bops-outcome :code) r))))
  (is (= :error (:outcome (b/parse-release 0 (js "{\"ok\":true,\"outcome\":\"landed\"}"))))
      "another command's positive outcome is not a release")
  (is (re-find #"bops release" (:error (b/parse-release 2 nil)))))

(deftest land-parse
  (let [r (b/parse-land 0 (js "{\"ok\":true,\"id\":\"bd-a1\",\"outcome\":\"landed\",
                                \"landed\":{\"ref\":\"https://x/pr/1\",\"sha\":\"abc\",\"run\":\"01RUN\",\"at\":\"t\"}}"))]
    (is (= :landed (:outcome r)))
    (is (= "https://x/pr/1" (get-in r [:landed :ref]))))
  (let [r (b/parse-land 3 (js "{\"ok\":false,\"id\":\"bd-a1\",\"outcome\":\"lost\",\"holder\":{\"actor\":\"x\"},
                                \"reason\":\"bd-a1 is no longer held by this run\"}"))]
    (is (= :lost (:outcome r)))
    (is (= {:actor "x"} (:holder r))))
  (let [r (b/parse-land 3 (js "{\"ok\":false,\"id\":\"bd-a1\",\"outcome\":\"refused\",\"reason\":\"bd-a1 is not held by this run\"}"))]
    (is (= :refused (:outcome r)))
    (is (= "bd-a1 is not held by this run" (:reason r))))
  (let [r (b/parse-land 1 (js "{\"ok\":false,\"error\":\"precondition: sha is not hex\",\"code\":\"\"}"))]
    (is (= :error (:outcome r)))
    (is (= "precondition: sha is not hex" (:error r)))))

(deftest sync-parse
  (let [r (b/parse-sync 0 (js "{\"ok\":true,\"pulled\":true,\"pushed\":false,\"revision\":\"r1\",\"unpublished\":2,
                                \"stranded_holds\":[],\"lost_claims\":[\"bd-a1\"],\"warnings\":[\"w\"]}"))]
    (is (true? (:synced r)))
    (is (= [true false] ((juxt :pulled :pushed) r)))
    (is (= ["bd-a1"] (:lost-claims r)))
    (is (= ["w"] (:warnings r))))
  (let [r (b/parse-sync 1 (js "{\"ok\":false,\"code\":\"unreachable\",\"error\":\"remote not reached\"}"))]
    (is (= [false true "unreachable"] ((juxt :synced :offline :code) r))))
  (is (false? (:synced (b/parse-sync 2 nil)))))

;; --- stubbed helpers: each function on lost and could-not-run ------------

(def ^:private cant-run {:exit 1 :err "" :doc {:ok false :code "unreachable" :error "remote not reached" :outcome "unreachable"}})
(def ^:private lost     {:exit 3 :err "" :doc {:ok false :outcome "lost" :holder {:actor "x"}}})
(def ^:private released {:exit 0 :err "" :doc {:ok true :outcome "released" :id "bd-a1"}})
(def ^:private landed   {:exit 0 :err "" :doc {:ok true :outcome "landed" :id "bd-a1" :landed {:ref "u" :sha "s"}}})

(defn- stub-bops
  "A bops stub answering by command, recording every call in `calls`."
  [calls answers]
  (fn [_ & args] (swap! calls conj (vec args)) (get answers (first args))))

(defn- thrown [f] (try (f) nil (catch clojure.lang.ExceptionInfo e e)))

(def ^:private task {:id "bd-a1" :issue_type "task"})
(def ^:private pr   {:ref "https://github.com/o/r/pull/7" :sha "0123abc"})

(deftest block-and-gate
  (doseq [[f reason] [[b/block! "blocked"] [b/gate-human! "human"]]]
    (let [calls (atom [])]
      (with-redefs [b/bops (stub-bops calls {"release" released})
                    b/bd   (fn [& _] {})]
        (is (= :released (:outcome (f {} "bd-a1" "why"))))
        (is (= ["release" "bd-a1" "--reason" reason] (take 4 (first @calls))))
        (is (= 1 (count @calls)))))
    (let [bd-calls (atom [])]
      (with-redefs [b/bops (stub-bops (atom []) {"release" lost})
                    b/bd   (fn [& args] (swap! bd-calls conj (vec args)) {})]
        (is (= :lost (:outcome (f {} "bd-a1" "why"))))
        (is (empty? @bd-calls) "a displaced holder writes nothing")))
    (let [bd-calls (atom [])]
      (with-redefs [b/bops (stub-bops (atom []) {"release" cant-run})
                    b/bd   (fn [& args] (swap! bd-calls conj (vec args)) {})]
        (let [e (thrown #(f {} "bd-a1" "why"))]
          (is (= "bops release could not run" (ex-message e)))
          (is (= "unreachable" (:code (ex-data e)))))
        (is (empty? @bd-calls))))))

;; block!'s BLOCKER text rides in the release itself: one write, no raw bd.
(deftest block-sends-its-blocker-through-release
  (let [calls (atom []) bd-calls (atom [])]
    (with-redefs [b/bops (fn [cfg & args] (swap! calls conj [(:in cfg) (vec args)]) released)
                  b/bd   (fn [& args] (swap! bd-calls conj (vec args)) {})]
      (is (= :released (:outcome (b/block! {:actor "k"} "bd-a1" "waiting on key"))))
      (is (= [["BLOCKER: waiting on key" ["release" "bd-a1" "--reason" "blocked" "--file" "-"]]] @calls)
          "the text goes on stdin, asked for with --file -")
      (is (empty? @bd-calls) "no raw bd note write")))
  (let [bd-calls (atom [])]
    (with-redefs [b/bops (fn [_ & _] lost)
                  b/bd   (fn [& args] (swap! bd-calls conj (vec args)) {})]
      (is (= :lost (:outcome (b/block! {} "bd-a1" "why"))))
      (is (empty? @bd-calls))))
  (doseq [blank [nil "" "   \n"]]
    (let [calls (atom [])]
      (with-redefs [b/bops (fn [_ & args] (swap! calls conj (vec args)) released)
                    b/bd   (fn [& _] (throw (ex-info "bd called" {})))]
        (is (= "block! needs a reason" (ex-message (thrown #(b/block! {} "bd-a1" blank)))))
        (is (empty? @calls) "a blank reason is refused before bops runs")))))

(deftest bops-passes-in-as-stdin
  (let [seen (atom nil)]
    (with-redefs [p/sh (fn [opts & cmd] (reset! seen [opts cmd]) {:exit 0 :out "{}" :err ""})]
      (b/bops {:in "BLOCKER: x"} "release" "bd-a1")
      (is (= "BLOCKER: x" (:in (first @seen))))
      (b/bops {} "release" "bd-a1")
      (is (not (contains? (first @seen) :in)) "no :in, no stdin"))))

;; babashka.process/sh takes the program and its args as separate strings: a
;; seq passed as one argument is run as a program named "(bops ...)".
(deftest sh-gets-the-command-as-strings
  (let [seen (atom [])]
    (with-redefs [p/sh (fn [_ & cmd] (swap! seen conj (vec cmd)) {:exit 0 :out "{}" :err ""})]
      (b/bops {:bops-path "/x/bops"} "release" "bd-a1")
      (b/bd "show" "bd-a1")
      (b/lint {:actor "k" :machine-label "m"} "bd-a1")
      (is (= [["/x/bops" "release" "bd-a1" "--json"]
              ["bd" "show" "bd-a1" "--json"]
              ["scripts/bd-lint-claim.sh" "bd-a1" "--json" "--no-sync" "--actor" "k" "--machine" "m"]]
             @seen)))))

(deftest gate-human-writes-the-gate-and-no-label
  (let [bd-calls (atom [])]
    (with-redefs [b/bops (stub-bops (atom []) {"release" released})
                  b/bd   (fn [& args] (swap! bd-calls conj (vec args)) {})]
      (b/gate-human! {} "bd-a1" "why")
      (is (= [["gate" "create" "--type" "human" "--blocks" "bd-a1" "--reason" "why"]] @bd-calls)
          "bops release --reason human writes needs-human; no second label write"))))

(deftest session-start-skips-both-needs-human-spellings
  (with-redefs [b/bops (stub-bops (atom []) {"sync" {:exit 0 :err "" :doc {:ok true :pulled true :pushed true}}})
                b/bd   (fn [& args]
                         (case (vec args)
                           ["list" "--status" "in_progress"] []
                           ["ready"] [{:id "bd-h" :priority 0 :status "open" :labels ["needs-human"]}
                                      {:id "bd-l" :priority 0 :status "open" :labels ["needs:human"]}
                                      {:id "bd-ok" :priority 2 :status "open" :labels []}]
                           {}))]
    (let [r (b/session-start {:actor "me" :machine-label "debian-box"})]
      (is (= "bd-ok" (get-in r [:resume :id]))))))

(deftest close-lands-then-releases-done
  (let [calls (atom [])]
    (with-redefs [b/bops (stub-bops calls {"land" landed "release" released})
                  b/bd   (fn [& _] (throw (ex-info "close! must not call raw bd" {})))]
      (let [r (b/close! {} task pr)]
        (is (= :released (:outcome r)))
        (is (= :landed (get-in r [:land :outcome]))))
      (is (= [["land" "bd-a1" "--ref" (:ref pr) "--sha" (:sha pr)]
              ["release" "bd-a1" "--reason" "done"]] @calls))))
  (let [calls (atom [])]
    (with-redefs [b/bops (stub-bops calls {"land" lost})]
      (is (= :lost (:outcome (b/close! {} task pr))))
      (is (= 1 (count @calls)) "no release after a lost landing")))
  (with-redefs [b/bops (stub-bops (atom []) {"land" landed "release" lost})]
    (is (= :lost (:outcome (b/close! {} task pr)))))
  (with-redefs [b/bops (stub-bops (atom []) {"land" cant-run})]
    (is (= "bops land could not run" (ex-message (thrown #(b/close! {} task pr))))))
  (with-redefs [b/bops (stub-bops (atom []) {"land" landed "release" cant-run})]
    (let [e (thrown #(b/close! {} task pr))]
      (is (= "bops release could not run" (ex-message e)))
      (is (= "unreachable" (:code (ex-data e))))))
  (let [calls (atom [])]
    (with-redefs [b/bops (stub-bops calls {"land" landed "release" released})]
      (is (= "agents do not close epics" (ex-message (thrown #(b/close! {} {:id "bd-e" :issue_type "epic"} pr)))))
      (is (some? (thrown #(b/close! {} task {:ref "u"}))) "close! needs a sha")
      (is (empty? @calls)))))

(deftest sync-never-throws
  (with-redefs [b/bops (stub-bops (atom []) {"sync" cant-run})]
    (is (= [false true "unreachable"] ((juxt :synced :offline :code) (b/sync! {}))))))

(let [{:keys [fail error]} (run-tests)]
  (System/exit (if (zero? (+ fail error)) 0 1)))
