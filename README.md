# Search Engine
This search engine allows you to index all types of documents, no matter where they are stored. It supports both traditional text search and a user-customizable use of text embeddings.

I personally use it to index millions of documents of different types, which results in >50GB of raw text. Despite this, most search queries are pretty fast.

Multiple document stores can be searched with this tool:
- Anything that can be mounted via a network mount
- Anything that can be synced to a local directory, like Sharepoint/OneDrive
- GitLab repositories, including wikis, PRs and issues
- Confluence Spaces
- Public Websites

Basically, the tool extracts text from all documents in these places, makes them searchable and links to them.

## Setup instructions
1. Create a GitLab Application. This is used to log people in and check if they are in the correct group, as specified by the `ALLOWED_GITLAB_GROUP_ID` variable.
2. Create the configuration and `.env` file.
3. Copy over the Docker Compose file and make any adjustments deemed necessary.

<details>
<summary>Click here for more details</summary>

### 1. GitLab Application (login provider)
Create an application with `read_api`, `read_user` and `openid` permission on GitLab (`https://gitlab.example.com/-/user_settings/applications`) (Preferences -> Applications -> Add new application).
If your server will expose the service on `https://my-domain.example.com:8090`, you should add `https://my-domain.example.com:8090/callback` to the allowed URLs in the GitLab application configuration.

Then put the host info and credentials you get after creating the application into the `.env` file:

```
# Gitlab instance
HOST_EXTERNAL_URL=http://<url-to-my-server>:8090
GITLAB_INSTANCE_URL=https://<gitlab-instance-url>
GITLAB_APPLICATION_ID=
GITLAB_APPLICATION_SECRET=
# GitLab Group ID of users which are allowed to log in (it's an integer)
ALLOWED_GITLAB_GROUP_ID=12345
```

### 2. Configuration
First, we need a configuration file. There is [an example configuration file](example-config.yml) with a lot of comments that explain how to use it. Basically, the configuration file defines which places are searchable, and how to access them.

Additionally, set up an `.env` file like this:

```
# Master Key that is used for logging into Meilisearch
# Must have sufficient complexity, otherwise Meilisearch just rejects it.
MEILI_MASTER_KEY=
# GitLab API Key that is used for cloning repositories and indexing issues/PRs.
# It requires the read_api, api, and ai_features scopes.
GITLAB_API_KEY=glpat-...

# GitLab OAuth settings. Explained in GitLab Application section in README
HOST_EXTERNAL_URL=http://my-cool-search.example.com
GITLAB_INSTANCE_URL=https://gitlab.example.com
GITLAB_APPLICATION_ID=
GITLAB_APPLICATION_SECRET=
# Users must be in this group to access the search tool, otherwise they are denied access. This should be a number, not the group name.
ALLOWED_GITLAB_GROUP_ID=

# Custom Environment variables that will also be available
# when evaluating e.g. the mount commands in the config file
NAS_USER=
NAS_PW=
```

### 3. Docker Setup
Now put the `docker-compose.yml` file and `.env` file in the same directory on your server that will host the service.

Ensure you are logged into the container registry:

Then, to start the server, run this:
```
docker compose up
```

Now the server should be available. It will take some time to index stuff.

## Permissions
This search engine is built with permission management in mind. It will index all available documents, but at search time, only the ones a user has access to will be returned.

The way this works is the following: every indexing item (e.g. a network mount, a git repo etc.) is associated with a "permission tag" (e.g. `ORG-Gitlab`).

Then, we define permission groups that have multiple tags associated with them. A user can be part of a group, and their group memberships define the tags they have access to.

We can give one permission group to a user by default, so any newly logged in user has access to a few basic resources (e.g. repositories in the GitLab group that is required for loggin in).

To edit user permissions, an admin user (those that have their numeric GitLab User ID in `admin_gitlab_ids` in the config) can go to `http://my.search.host:8090/admin` and edit permissions.


</details>

## Development
To add a new source of documents, please add it to the scraper package, and then initialize your scraper from the [`indexer/main.go`](indexer/main.go) file with values from the configuration.

### Design Justifications
If you are reasonable, you will be surprised by the number of different services defined in the [`docker-compose.yml`](docker-compose.yaml) file. Let me explain why this is necessary.

TL;DR: `indexer` and `searcher` are split to reduce attack surface.

Services:
- `meilisearch` & `tika`: services maintained by their own teams that we use unmodified
- `indexer`: indexes network mounts, GitLab instances etc. Initially, this was a "background thread" of the search service, however, it was split out due to security considerations: if there was some kind of path traversal vulnerability in our user-facing backend code, they might be able to access any file on a NAS, as that is mounted into the same container. If we have a separate indexer that is not exposed to the outside world, the attack surface is reduced.
- `searcher`: Takes in search requests and forwards them to Meilisearch. It also does some post-processing on the search results to reduce bandwidth used (as in: only send back the most relevant section)
- `embedder`: Generates text embeddings if enabled, needs a GPU


### Updating Meilisearch
If there is a new Meilisearch version, it is possible that the index format is no longer supported. You could [migrate it via a dump](https://www.meilisearch.com/docs/learn/update_and_migration/updating), or just ignore that and remove the old data (as in, just `rm -rf meili_data`).

Since text extraction is usually cached, only the reindexing of the content is required.
Also, don't just update Meilisearch and assume the search tool will still work - likely, the client library needs to be updated as well.
