"""APIClient adapted for this backend's auth and URL conventions.

USAGE.md calls this out as the one piece you almost always have to edit, and
that held. Four things differ from the bundled Django/DRF client:

  auth      JWT bearer, not session + CSRF. Registration returns a token
            directly, so there is no separate login round trip for a new user.

  urls      No trailing slashes. DRF's router appends them; chi treats
            /v1/notes/ and /v1/notes as different routes, and the former 404s.

  update    PATCH with a required If-Match precondition, not PUT. This is the
            interesting one -- see NoteHelpers below.

  errors    RFC 9457 problem+json. The detail lives in `detail`, not in DRF's
            field-keyed dict, so failures print something readable.

Two extra methods exist that the bundled client has no equivalent for:
`expect_status`, because most of the rules worth asserting here are *refusals*
(404 for an invisible note, 409 for a stale write), and `raw`, which returns
the response instead of raising so those can be checked.
"""

import json
import os

import requests

from utils.decorators import with_perf, with_stack_trace
from utils.misc import random_string

# Every fixture account uses this. Long enough to pass the 12-character
# minimum; short enough to stay under bcrypt's 72-byte cap.
FIXTURE_PASSWORD = "nocrud-fixture-password"


def base_url():
    """Where the API lives.

    NOCRUD_BASE_URL wins when set, so flows can run against a deployment. The
    bundled client only ever builds a localhost URL from APP_PORT, which is
    right for provisioned flows and useless for a smoke test against a real
    host.
    """
    override = os.getenv("NOCRUD_BASE_URL")
    if override:
        return f"{override.rstrip('/')}/v1"
    port = os.getenv("APP_PORT", "8080")
    return f"http://localhost:{port}/v1"


def new_api_client_with_new_random_user(label="user"):
    """Register a fresh user and return a client authenticated as them.

    The randomised email matters: flows run against a database that may already
    hold accounts, and registration is case-insensitively unique, so a fixed
    address would 409 on the second run.
    """
    api = APIClient()
    api.register(
        email=f"{label}-{random_string(10)}@example.test".lower(),
        password=FIXTURE_PASSWORD,
        display_name=label,
    )
    return api


def new_api_clients(*labels):
    """Several authenticated users at once.

    The multi-user flows are the point of this runner: nearly every rule here
    is about what one user may do to another's note.
    """
    return [new_api_client_with_new_random_user(label) for label in labels]


class APIClient:
    def __init__(self, base=None):
        self.base_url = base or base_url()
        self.session = requests.Session()
        self.username = ""
        self.token = ""
        self.user_id = ""

    # --- auth ---------------------------------------------------------------

    def register(self, email, password, display_name):
        """Create an account and adopt its token."""
        res = self.session.post(
            f"{self.base_url}/auth/register",
            json={
                "email": email,
                "password": password,
                "display_name": display_name,
            },
        )
        print_on_fail_status(res)
        res.raise_for_status()
        return self._adopt(res.json(), email)

    def login(self, email, password):
        res = self.session.post(
            f"{self.base_url}/auth/login",
            json={"email": email, "password": password},
        )
        print_on_fail_status(res)
        res.raise_for_status()
        return self._adopt(res.json(), email)

    def _adopt(self, payload, email):
        self.token = payload["access_token"]
        self.user_id = payload["user"]["id"]
        self.username = payload["user"].get("display_name", email)
        self.email = email
        # Bearer on the session, so every later request carries it.
        self.session.headers.update({"Authorization": f"Bearer {self.token}"})
        print(f"\nAPI Client Created With {self.username} ({self.user_id})")
        return payload

    # --- CRUD, in the shape utils/crud.py expects ---------------------------

    @with_perf("Create:")
    @with_stack_trace
    def create_object(self, endpoint, data):
        res = self.session.post(f"{self.base_url}/{endpoint}", json=data)
        print_on_fail_status(res)
        res.raise_for_status()
        return res.json()

    @with_perf("Get:")
    @with_stack_trace
    def get_object_by_id(self, endpoint, object_id=None, silent=False):
        url = f"{self.base_url}/{endpoint}"
        if object_id:
            url += f"/{object_id}"
        res = self.session.get(url)
        print_on_fail_status(res)
        res.raise_for_status()
        if not silent:
            print(f"Objects retrieved: {res.json()}")
        return res.json()

    @with_perf("Update:")
    @with_stack_trace
    def update_object_by_id(self, endpoint, object_id, data, if_match=None):
        """PATCH, with an If-Match precondition when the resource needs one.

        The bundled client PUTs a whole object back. This API takes a partial
        PATCH and *requires* If-Match on notes, so a blind PUT would be
        rejected with 428 rather than succeeding.
        """
        headers = {}
        if if_match is not None:
            headers["If-Match"] = str(if_match)
        res = self.session.patch(
            f"{self.base_url}/{endpoint}/{object_id}", json=data, headers=headers
        )
        print_on_fail_status(res)
        res.raise_for_status()
        return res.json()

    @with_perf("Delete:")
    @with_stack_trace
    def delete_object_by_id(self, endpoint, object_id):
        url = f"{self.base_url}/{endpoint}"
        if object_id:
            url += f"/{object_id}"
        res = self.session.delete(url)
        print_on_fail_status(res)
        res.raise_for_status()
        print(f"Object with ID {object_id} deleted successfully.")

    # --- freeform -----------------------------------------------------------

    @with_perf("GET:")
    @with_stack_trace
    def get(self, endpoint, silent=False):
        res = self.session.get(f"{self.base_url}/{endpoint}")
        print_on_fail_status(res)
        res.raise_for_status()
        if not silent:
            print(f"{self.username}: retrieved {res.json()}")
        return res.json()

    @with_perf("POST:")
    @with_stack_trace
    def post(self, endpoint, data=None, silent=False):
        res = self.session.post(f"{self.base_url}/{endpoint}", json=data)
        print_on_fail_status(res)
        res.raise_for_status()
        # 204 is common here (membership changes, password change), and .json()
        # on an empty body raises.
        payload = res.json() if res.content else None
        if not silent:
            print(f"{self.username}: POST {endpoint} -> {res.status_code}")
        return payload

    @with_perf("PATCH:")
    @with_stack_trace
    def patch(self, endpoint, data=None, headers=None, silent=False):
        res = self.session.patch(
            f"{self.base_url}/{endpoint}", json=data, headers=headers or {}
        )
        print_on_fail_status(res)
        res.raise_for_status()
        payload = res.json() if res.content else None
        if not silent:
            print(f"{self.username}: PATCH {endpoint} -> {res.status_code}")
        return payload

    @with_perf("DELETE:")
    @with_stack_trace
    def delete(self, endpoint, silent=False):
        res = self.session.delete(f"{self.base_url}/{endpoint}")
        print_on_fail_status(res)
        res.raise_for_status()
        if not silent:
            print(f"{self.username}: DELETE {endpoint} -> {res.status_code}")

    # --- assertions on refusals ---------------------------------------------

    def raw(self, method, endpoint, data=None, headers=None):
        """Send a request and return the response WITHOUT raising.

        Needed because most rules in this API are refusals. The bundled
        assert_fail catches a RequestException, which tells you *something*
        failed but not with which status -- and here 403 versus 404 is the
        whole point of the rule.
        """
        return self.session.request(
            method.upper(),
            f"{self.base_url}/{endpoint}",
            json=data,
            headers=headers or {},
        )

    def expect_status(self, method, endpoint, expected, data=None, headers=None, why=""):
        """Assert an exact status code. Raises on mismatch, which is the
        runner's failure signal."""
        res = self.raw(method, endpoint, data=data, headers=headers)
        if res.status_code != expected:
            raise AssertionError(
                f"{method.upper()} {endpoint} as {self.username}: "
                f"expected {expected}, got {res.status_code}"
                + (f" — {why}" if why else "")
                + f"\n  body: {res.text[:400]}"
            )
        print(f"✔ {method.upper()} {endpoint} -> {expected} as expected"
              + (f" ({why})" if why else ""))
        return res


def print_on_fail_status(response):
    if response.status_code > 299:
        print(f"🔴 ERROR {response.status_code}: {response.text}")
        try:
            body = response.json()
            # RFC 9457: the useful message is `detail`, with per-field problems
            # under `errors`.
            if "detail" in body:
                print(f"🔍 {body.get('title', '')}: {body['detail']}")
                if body.get("errors"):
                    print(f"🔍 fields: {json.dumps(body['errors'], indent=2)}")
            else:
                print("🔍 Error Details:", json.dumps(body, indent=2))
        except json.JSONDecodeError:
            print("⚠️ Response is not JSON:", response.text)
