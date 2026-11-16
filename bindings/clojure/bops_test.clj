;; Tests for the generated namespace bops (bops.clj, from `bops gen clj`).
;; Needs Babashka, not bops. Run from this directory: bb bops_test.clj
(require '[clojure.test :refer [deftest is run-tests]]
         '[babashka.process :as p]
         '[bops])

(defn- thrown [f] (try (f) nil (catch clojure.lang.ExceptionInfo e (ex-message e))))

(deftest argv-follows-the-manifest
  (is (= ["claim" "x-1"] (bops/argv "claim" "x-1" {})))
  (is (= ["claim" "x-1" "--force"] (bops/argv "claim" "x-1" {:force true})))
  (is (= ["stale" "--all-machines=false"] (bops/argv "stale" nil {:all-machines false})))
  (is (= ["next" "--caps" "gpu,net" "--n" "5"] (bops/argv "next" nil {:caps ["gpu" "net"] :n 5})))
  (is (= ["land" "x-1" "--ref" "u" "--sha" "s" "--verify"] (bops/argv "land" "x-1" {:ref "u" :sha "s" :verify true})))
  (is (= ["release" "x-1" "--reason" "blocked" "--file" "-"] (bops/argv "release" "x-1" {:reason "blocked" :text "t"}))
      "optional text is asked for with --file -")
  (is (= ["note" "x-1"] (bops/argv "note" "x-1" {:text "t"})) "required text is read from stdin unasked"))

(deftest argv-refuses-a-bad-call
  (is (= "bops land needs :sha" (thrown #(bops/argv "land" "x-1" {:ref "u"}))))
  (is (= "bops claim takes no option :forse" (thrown #(bops/argv "claim" "x-1" {:forse true}))))
  (is (= "bops claim needs a bead id" (thrown #(bops/argv "claim" nil {}))))
  (is (= "bops note needs text" (thrown #(bops/argv "note" "x-1" {}))))
  (is (= "bops has no command nope" (thrown #(bops/argv "nope" nil {})))))

(deftest fns-shell-out-with-text-on-stdin
  (let [seen (atom [])]
    (with-redefs [p/sh (fn [opts & cmd] (swap! seen conj [(:in opts) (vec cmd)]) {:exit 3 :out "{\"ok\":false,\"outcome\":\"lost\"}" :err ""})]
      (is (= {:exit 3 :err "" :doc {:ok false :outcome "lost"}} (bops/note {} "x-1" "NEXT: x")))
      (bops/release {:bops-path "/b"} "x-1" {:reason "scope"})
      (is (= [["NEXT: x" ["bops" "note" "x-1" "--json"]]
              [nil ["/b" "release" "x-1" "--reason" "scope" "--json"]]]
             @seen)))))

(def ^:private claimed
  {:contract 1 :command "claim" :ok true :id "x-1" :run "R" :outcome "claimed" :warnings []})

(deftest schemas-check-types-and-required-fields
  (is (bops/valid? "claim" claimed))
  (is (bops/valid? "claim" (assoc claimed :new_field 1)) "additive fields pass")
  (is (not (bops/valid? "claim" (assoc claimed :ok "yes"))))
  (is (not (bops/valid? "claim" (dissoc claimed :outcome))))
  (is (bops/valid? "claim" (assoc claimed :holder nil)) "an optional field may be null")
  (is (not (bops/valid? "doctor" {:contract 1 :command "doctor" :ok true :remote "o" :probe_ref "r" :failed [] :warnings []
                                  :checks [{:id "origin" :status "maybe" :detail ""}]}))
      "enum values are checked")
  (is (bops/valid? "doctor" {:contract 1 :command "doctor" :ok true :remote "o" :probe_ref "r" :failed [] :warnings []
                             :checks [{:id "origin" :status "ok" :detail ""}]}))
  (is (some? (bops/explain "claim" (dissoc claimed :id)))))

(let [{:keys [fail error]} (run-tests)]
  (System/exit (if (zero? (+ fail error)) 0 1)))
