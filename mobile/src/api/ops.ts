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

export const SubscribeToken = graphql(`
  mutation SubscribeToken($id: String!) {
    dund {
      subscribeToken(id: $id) {
        principal
        timestamp
        hmac
      }
    }
  }
`);

export const SessionEventsLive = graphql(`
  subscription SessionEventsLive($session: String!, $after: Long, $principal: String!, $timestamp: Long!, $hmac: String!) {
    dund_sessionEvents(session: $session, after: $after, principal: $principal, timestamp: $timestamp, hmac: $hmac) {
      event
    }
  }
`);

export const Slash = graphql(`
  mutation Slash($id: String!, $line: String!) {
    dund {
      control(id: $id, body: { kind: control, target: "slash", action: $line }) {
        ok
      }
    }
  }
`);

export const ForgetSession = graphql(`
  mutation ForgetSession($id: String!) {
    dund {
      forgetSession(id: $id) {
        ok
      }
    }
  }
`);

export const AskModels = graphql(`
  mutation AskModels($id: String!) {
    dund {
      control(id: $id, body: { kind: control, target: "models", action: "" }) {
        ok
      }
    }
  }
`);

export const SwitchModel = graphql(`
  mutation SwitchModel($id: String!, $url: String!, $model: String!) {
    dund {
      switchModel(id: $id, body: { url: $url, model: $model }) {
        ok
      }
    }
  }
`);

export const GetUI = graphql(`
  query GetUI($id: String!) {
    dund {
      getUI(id: $id) {
        state
      }
    }
  }
`);

export const PutUI = graphql(`
  mutation PutUI($id: String!, $state: JSON!) {
    dund {
      putUI(id: $id, body: { state: $state }) {
        ok
      }
    }
  }
`);
