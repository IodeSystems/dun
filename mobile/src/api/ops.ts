import { graphql } from '@/gql';

export const ListSessions = graphql(`
  query ListSessions {
    dund {
      listSessions {
        items {
          id
          workspace
          running
          lastSeq
        }
      }
    }
  }
`);

export const SessionEvents = graphql(`
  query SessionEvents($id: String!, $after: Long, $wait: Long) {
    dund {
      sessionEvents(id: $id, after: $after, wait: $wait) {
        events
        snapshot
        last
      }
    }
  }
`);

export const CreateSession = graphql(`
  mutation CreateSession($workspace: String!) {
    dund {
      createSession(body: { workspace: $workspace }) {
        id
      }
    }
  }
`);

export const SendMessage = graphql(`
  mutation SendMessage($id: String!, $content: String!) {
    dund {
      sendMessage(id: $id, body: { content: $content }) {
        ok
      }
    }
  }
`);

export const Answer = graphql(`
  mutation Answer($id: String!, $value: String!) {
    dund {
      answer(id: $id, body: { value: $value }) {
        ok
      }
    }
  }
`);
