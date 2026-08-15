"""CRUD flow for teams.

No If-Match here: teams carry no version, because nothing about a team name is
worth a concurrency conflict. The contrast with notes is the point -- the
precondition exists where concurrent edits destroy work, not everywhere.
"""

from utils.api_client import new_api_client_with_new_random_user
from utils.crud import crud_exec


def create(api):
    res = api.create_object("teams", {"name": "Platform"})
    return res["id"]


def crud():
    api = new_api_client_with_new_random_user("team-admin")
    return crud_exec(
        "teams",
        api,
        create,
        {"fieldName": "name", "length": 12, "newValue": None, "ifMatchField": None},
    )
