import argparse
import time

### Local Utils
from runners.main import crud_flows_runners, request_flows_runners
from utils.db_client import DBClient
from utils.printing import print_group_separator
from utils.collector import collect_flows_by_folder
from utils import perf
##### Import Manually Registered Flows
from flows.crud import note, team
from flows.confirm_business_logic import concurrency, sharing, teams_and_accounts

### Manually Registered

# Multi-user, multi-endpoint flows asserting the real rules. This is the
# highest-value output -- see nocrud/INVENTORY.md for the rule each maps to.
REQUEST_FLOWS = {
    # Access
    "private_note": sharing.private_note_stays_private,
    "grant_revoke": sharing.granting_and_revoking_access,
    "no_reshare": sharing.editors_cannot_reshare,
    "cross_team": sharing.cross_team_sharing,
    "strongest_grant": sharing.strongest_grant_wins,
    "foreign_team": sharing.sharing_with_a_foreign_team_is_refused,
    # Concurrency
    "conflict": concurrency.concurrent_edit_is_rejected_not_merged,
    "precondition": concurrency.precondition_is_mandatory,
    "etag_forms": concurrency.quoted_and_bare_versions_both_work,
    "soft_delete": concurrency.soft_delete_hides_and_owner_restores,
    # Teams and accounts
    "last_admin": teams_and_accounts.last_admin_cannot_strand_the_team,
    "team_privacy": teams_and_accounts.team_membership_is_concealed_from_outsiders,
    "team_delete": teams_and_accounts.deleting_a_team_revokes_its_grants,
    "accounts": teams_and_accounts.account_rules,
}

# Single-endpoint create/read/update/delete flows.
CRUD_FLOWS = {
    "note": note.crud,
    "team": team.crud,
}


def main():
    # Automatic registration - Currently flow functions must end in _flow to be collected
    collectedFlows: dict = {}
    try:
        collectedFlows = collect_flows_by_folder("flows/auto_registered")
    except Exception as e:
        print("Error collecting flows:", e)

    parser = argparse.ArgumentParser(description="Run backend user flow simulations.")
    parser.add_argument(
        "-s",
        "--serial",
        action="store_true",
        help="Run flows serially",
    )
    parser.add_argument(
        "--reset-db",
        action="store_true",
        help=(
            "Before a serial run, drop and rebuild the target database. "
            "Local development only -- this destroys data, so it is opt-in "
            "rather than implied by --serial."
        ),
    )
    parser.add_argument(
        "-perf",
        "--perf",
        action="store_true",
        help="Persist request timings for this run and compare against the baseline",
    )
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument(
        "-f",
        "--flows",
        nargs="+",  # Allow multiple flows
        choices=list(REQUEST_FLOWS.keys())
        + list(CRUD_FLOWS.keys())
        + list(collectedFlows.keys()),
        help="The name(s) of the flow(s) to run",
    )

    group.add_argument(
        "-coll",
        "--collected",
        action="store_true",
        help="Run all collected flows",
    )

    group.add_argument(
        "-req",
        "--request_flows",
        action="store_true",
        help="Run all request flows",
    )

    group.add_argument(
        "-crud",
        "--crud",
        action="store_true",
        help="Run all crud flows",
    )
    group.add_argument(
        "-l",
        "--list",
        action="store_true",
        help="List all flows",
    )

    args = parser.parse_args()

    #####################################
    # Options that dont run any flows
    #####################################
    if args.list:
        print("\nCRUD_FLOWS:")
        print(*(f"\n{k}: {v}" for k, v in CRUD_FLOWS.items()))
        print("\n\nREQUEST FLOWS:")
        print(*(f"\n{k}: {v}" for k, v in REQUEST_FLOWS.items()))
        print("\n\nCOLLECTED FLOWS:")
        print(*(f"\n{k}: {v}" for k, v in collectedFlows.items()))
        return

    #####################################
    # "Main"
    #####################################

    perf_run_dir = None
    if args.perf:
        import os

        os.environ["NOCRUD_PERF"] = "1"
        # Init in the parent so child processes (parallel mode) inherit the run id.
        run_id = perf.init_run()
        perf_run_dir = perf.RUNS_DIR / run_id
        print(f"⏱  Perf collection on — run id {run_id}")

    start_time = time.perf_counter()

    is_parallel = True
    if args.serial:
        is_parallel = False
        # Serial mode runs against an app someone else started, which may be a
        # deployed one. Rebuilding the schema is therefore explicitly requested,
        # never implied: `smoke.sh` runs serially against production, and a
        # reset there would drop every table.
        if args.reset_db:
            print_group_separator("Initial Setup — resetting database")
            db = DBClient()
            db.reset()
        else:
            print_group_separator("Serial run — using the existing app and data")
            print("(pass --reset-db to rebuild the schema first; local dev only)")

    # Filter and Run
    print_group_separator("Run Flows")
    flows_to_run = []
    allFlows = {**REQUEST_FLOWS, **CRUD_FLOWS, **collectedFlows}

    if args.request_flows:
        flows_to_run = REQUEST_FLOWS.keys()
        print(f"Flows to run: {flows_to_run}")
        flows = [(name, allFlows[name]) for name in flows_to_run]
        request_flows_runners(flows, parallel=is_parallel)

    if args.crud:
        flows_to_run = CRUD_FLOWS.keys()
        print(f"Flows to run: {flows_to_run}")
        flows = [(name, allFlows[name]) for name in flows_to_run]
        crud_flows_runners(flows, parallel=is_parallel)

    if args.collected:
        flows_to_run = collectedFlows.keys()
        print(f"Flows to run: {flows_to_run}")
        flows = [(name, allFlows[name]) for name in flows_to_run]
        request_flows_runners(flows, parallel=is_parallel)

    if args.flows:
        flows_to_run = args.flows  # Add explicitly specified flows
        print(f"Flows to run: {flows_to_run}")
        flows = [(name, allFlows[name]) for name in flows_to_run]
        request_flows_runners(flows, parallel=is_parallel)

    end_time = time.perf_counter()
    print("=" * 80)
    print(f"\nTest runner took: {end_time - start_time:.6f} seconds")

    if args.perf and perf_run_dir is not None:
        from perf_report import compare_and_print

        print_group_separator("Timings")
        compare_and_print(perf_run_dir)
        print(
            "\nSet this run as the baseline to compare against next time:\n"
            "  python perf_report.py --set-baseline"
        )


if __name__ == "__main__":
    main()
