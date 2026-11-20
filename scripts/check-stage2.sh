# Sourced by the stage 2 mode after the local stack has been created.
set +x
hub="$build/hub"
mark "stage 2 enrollment"
echo '+ hub code | docker compose exec -T link /link enroll'
"$hub" code | docker compose exec -T link /link enroll
echo '+ hub donors'
for _ in {1..80}; do
    if "$hub" donors >"$build/donors" && grep -q '"ready":true' "$build/donors"; then break; fi
    sleep .25
done
cat "$build/donors"
grep -q '"ready":true' "$build/donors"
donor=$(python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])' <"$build/donors")
echo '{}' >"$build/empty.json"
if [[ $mode == stage2-revoke ]]; then
    python3 -u scripts/check-stage2.py "$build" "$hub_pid" "$link" "$donor" revoke
    return
fi
mark "stage 2 smoke job"
echo '+ hub run DONOR empty.json -- /bin/sh -c "echo smoke"'
"$hub" run "$donor" "$build/empty.json" -- /bin/sh -c "echo smoke"
mark "stage 3 locked inputs"
echo '+ hub run DONOR manifests/flare.json -- verify commit and locked crate'
"$hub" run "$donor" manifests/flare.json -- /bin/sh -ec '
    git -C source rev-parse HEAD
    printf "%s  %s\n" 320119579fcad9c21884f5c4861d16174d0e06250625266f50fe6898340abefa crates/adler2-2.0.1.crate | sha256sum -c
    git -C source rev-parse HEAD >input-checks
' | tee "$root/manifest-job.log"
python3 -u scripts/check-stage2.py "$build" "$hub_pid" "$link" "$donor"
mark "7 stage 1 checks through the link"
sudo -n python3 -u scripts/checks.py "$build/runner/stage1" "$control/runner.sock" "$runner" "$WORK_IMAGE" \
    "$build/driver" "$HUB_SOCKET" "$donor" stage2
python3 -u scripts/check-stage2.py "$build" "$hub_pid" "$link" "$donor" revoke
