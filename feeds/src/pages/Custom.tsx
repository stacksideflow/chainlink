import { DispatchBinding } from '@chainlink/ts-helpers'
import React, { useEffect, useState } from 'react'
import { RouteComponentProps } from 'react-router-dom'
import { connect, MapDispatchToProps } from 'react-redux'
import { FeedConfig } from 'config'
import { Header } from 'components/header'
import { Aggregator, FluxAggregator } from 'components/aggregator'
import { aggregatorOperations } from '../state/ducks/aggregator'
import { useLocation } from 'react-router-dom'
import { parseQuery, uIntFrom } from 'utils'

interface OwnProps
  extends RouteComponentProps<{ pair: string; network?: string }> {}

interface DispatchProps {
  fetchOracleNodes: DispatchBinding<
    typeof aggregatorOperations.fetchOracleNodes
  >
}

interface Props extends OwnProps, DispatchProps {}

const Page: React.FC<Props> = ({ fetchOracleNodes }) => {
  const location = useLocation()
  const [config] = useState<FeedConfig>(
    parseConfig(parseQuery(location.search)),
  )

  useEffect(() => {
    fetchOracleNodes()
  }, [fetchOracleNodes])

  let content
  if (config && config.contractVersion === 3) {
    content = <FluxAggregator config={config} />
  } else if (config) {
    content = <Aggregator config={config} />
  } else {
    content = <>There was an error loading the page. Refresh to try again.</>
  }

  return (
    <>
      <div className="page-container-full-width">
        <Header />
      </div>
      <div className="page-wrapper network-page">{content}</div>
    </>
  )
}

/**
 * Hydrate a feed config into its internal representation
 *
 * @param config The config in map format
 */
function parseConfig(config: Record<string, string>): FeedConfig {
  return {
    ...((config as unknown) as FeedConfig),
    networkId: uIntFrom(config.networkId ?? 0),
    contractVersion: 2,
    decimalPlaces: uIntFrom(config.decimalPlaces ?? 0),
    heartbeat: uIntFrom(config.heartbeat ?? 0) ?? false,
    historyDays: uIntFrom(config.historyDays ?? 1),
  }
}

const mapDispatchToProps: MapDispatchToProps<DispatchProps, OwnProps> = {
  fetchOracleNodes: aggregatorOperations.fetchOracleNodes,
}

export default connect(null, mapDispatchToProps)(Page)
