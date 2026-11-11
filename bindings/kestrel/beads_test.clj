;; Smoke test for beads.clj: the namespace loads, and parse-claim maps the
;; `bops claim --json` shapes (bops/commands.json `claim`, SPEC §4) to results.
;; Needs no bd or bops. Run from the repo root:
;;   bb -Sdeps '{:deps {metosin/malli {:mvn/version "0.16.1"}}}' bindings/kestrel/beads_test.clj
(require '[clojure.test :refer [deftest is run-tests]]
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

(let [{:keys [fail error]} (run-tests)]
  (System/exit (if (zero? (+ fail error)) 0 1)))
