"""CRUD flow for notes.

The one non-obvious part is `ifMatchField`. Notes require an If-Match
precondition on every write, so the update leg reads the current version out of
the object and echoes it back. Without it the stock helper gets 428.
"""

from utils.api_client import new_api_client_with_new_random_user
from utils.crud import crud_exec


def create(api):
    res = api.create_object(
        "notes",
        {
            "title": "Quarterly planning",
            "body": "Agenda and open questions.",
            "tags": ["planning", "q3"],
        },
    )
    return res["id"]


def crud():
    api = new_api_client_with_new_random_user("note-owner")
    return crud_exec(
        "notes",
        api,
        create,
        {
            "fieldName": "title",
            "length": 20,
            "newValue": None,
            "ifMatchField": "version",
        },
    )
