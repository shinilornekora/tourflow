// Продакшен-сборка. Оптимизации (презентация 4 «Модульность и сборщики»):
//  - contenthash в именах файлов → долгий кэш в браузере, при изменении
//    кода меняется только имя изменившегося чанка;
//  - splitChunks: react и MUI в отдельных vendor-чанках — они меняются
//    реже, чем код приложения, и остаются в кэше между релизами;
//  - runtimeChunk: манифест модулей отдельно, иначе любое изменение
//    инвалидирует vendor-чанк;
//  - страницы грузятся лениво (React.lazy) → первый экран меньше;
//  - минификация JS (terser, встроен) и CSS, tree shaking (mode=production);
//  - hidden-source-map: карты есть для отладки ошибок, но браузер их не грузит.
const { merge } = require('webpack-merge');
const MiniCssExtractPlugin = require('mini-css-extract-plugin');
const CssMinimizerPlugin = require('css-minimizer-webpack-plugin');
const common = require('./webpack.common');

const plugins = [new MiniCssExtractPlugin({ filename: 'assets/[name].[contenthash:8].css' })];
if (process.env.ANALYZE) {
  const { BundleAnalyzerPlugin } = require('webpack-bundle-analyzer');
  plugins.push(new BundleAnalyzerPlugin());
}

module.exports = merge(common, {
  mode: 'production',
  devtool: 'hidden-source-map',
  output: {
    filename: 'assets/[name].[contenthash:8].js',
    chunkFilename: 'assets/[name].[contenthash:8].js',
    assetModuleFilename: 'assets/[name].[contenthash:8][ext]',
  },
  module: {
    rules: [{ test: /\.css$/, use: [MiniCssExtractPlugin.loader, 'css-loader'] }],
  },
  plugins,
  optimization: {
    moduleIds: 'deterministic',
    runtimeChunk: 'single',
    minimizer: ['...', new CssMinimizerPlugin()],
    splitChunks: {
      chunks: 'all',
      cacheGroups: {
        react: {
          test: /[\\/]node_modules[\\/](react|react-dom|scheduler|react-router|react-router-dom|@remix-run)[\\/]/,
          name: 'vendor-react',
          priority: 30,
        },
        mui: {
          test: /[\\/]node_modules[\\/](@mui|@emotion)[\\/]/,
          name: 'vendor-mui',
          priority: 20,
        },
        redux: {
          test: /[\\/]node_modules[\\/](@reduxjs|react-redux|redux|immer|reselect)[\\/]/,
          name: 'vendor-redux',
          priority: 10,
        },
      },
    },
  },
  performance: {
    hints: 'warning',
    // ~200 КБ gzip на первый экран; лимит по несжатому размеру.
    maxEntrypointSize: 700 * 1024,
    maxAssetSize: 400 * 1024,
  },
});
