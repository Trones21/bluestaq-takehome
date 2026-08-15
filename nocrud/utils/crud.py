from .fixtures import get_fixture_by_index
from .api_client import APIClient
from .misc import random_string
from typing import Callable, TypedDict, Optional


def simple_create(
    api: APIClient, endpoint: str, fixtureIndex: int, idField: str, fixtureName: str
):
    """Assumes that the object is not dependent on other obejcts (and needs no other modification before request is sent)"""
    obj = get_fixture_by_index(fixtureName, fixtureIndex)
    res = api.create_object(endpoint, obj)
    return res[idField]


def read(api: APIClient, endpoint, id):
    api.get_object_by_id(endpoint, id)
    return True


class UpdateDetails(TypedDict):
    fieldName: str
    length: int
    newValue: Optional[str | int]
    # Added for this backend. Names the field carrying the resource's current
    # version, which is echoed back as an If-Match precondition on write.
    #
    # The stock helper PUTs the whole object back with no precondition, which
    # this API answers with 428 Precondition Required -- it refuses
    # unconditional overwrites, because two people editing one note is the
    # normal case and last-write-wins silently destroys work. Omit the key and
    # the old behaviour is unchanged.
    ifMatchField: Optional[str]


def update(api: APIClient, endpoint, id, updateDetails: UpdateDetails):
    obj = api.get_object_by_id(endpoint, id, True)
    if updateDetails["newValue"] is not None:
        expected = updateDetails["newValue"]
    else:
        expected = random_string(updateDetails["length"])

    if_match = None
    version_field = updateDetails.get("ifMatchField")
    if version_field:
        if_match = obj[version_field]

    # Send only the changed field. The stock helper round-trips the whole
    # object, which a PATCH API rejects: read-only members like id and
    # created_at come back as unknown fields, and this service returns 400
    # rather than silently ignoring them.
    payload = {updateDetails["fieldName"]: expected}

    res = api.update_object_by_id(endpoint, id, payload, if_match=if_match)
    actual = res[updateDetails["fieldName"]]
    if expected == actual:
        return True
    print("Expected: ", expected, " Actual: ", actual)
    return False


def delete(api: APIClient, endpoint, id):
    api.delete_object_by_id(endpoint, id)
    return True


def crud_exec(
    endpoint: str, api: APIClient, createFunc: Callable[[APIClient], str], updateDetails
):
    crud = {}
    id = createFunc(api)
    crud["create"] = (
        True  # we would have gotten a stack trace and stopped execution if there was a failure so if we make it this far then we know its a success
    )
    crud["read"] = read(api, endpoint, id)
    crud["update"] = update(api, endpoint, id, updateDetails)
    crud["delete"] = delete(api, endpoint, id)
    return crud


def simpleGetAndCreate(api: APIClient, entityName, returnVal, id_field_name):
    returnValOptions = ["object", "id"]


# Creates an object and all prerequisite objects
def createPreRequisiteObjs(entityName):
    EntityFlows = {}
